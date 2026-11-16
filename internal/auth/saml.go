package auth

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	saml2 "github.com/russellhaering/gosaml2"
	"github.com/russellhaering/gosaml2/types"
	dsig "github.com/russellhaering/goxmldsig"
)

const (
	samlRequestLifetime = 10 * time.Minute
	emailFormat         = "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
	samlCookie          = "sw_saml"
)

var (
	errNoSAML       = errors.New("the org has no SAML identity provider")
	errUnsolicited  = errors.New("the response answers no sign-in that Stackwright started")
	errOutsideOrg   = errors.New("the email belongs to an account with no place in this org")
	errOtherBrowser = errors.New("the sign-in started in another browser")
	errOwner        = errors.New("an owner signs in another way, so the identity provider cannot act as an owner")
	errAssertion    = errors.New("the assertion is not valid now or not for Stackwright")
	errNotAnAddress = errors.New("the NameID is not an email address")
)

// SAML is the service provider for each org's SAML identity provider
// (adr/0045).
type SAML struct {
	Pool      *pgxpool.Pool
	PublicURL string
	Cookies   Cookies
}

func (s *SAML) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/saml/{org}/metadata", s.metadata)
	mux.HandleFunc("GET /auth/saml/{org}/start", s.start)
	mux.HandleFunc("POST /auth/saml/{org}/acs", s.acs)
	return mux
}

// SPEntityID is the entity id and metadata URL of an org's service
// provider; ACSURL is where its identity provider posts.
func SPEntityID(publicURL, org string) string { return publicURL + "/auth/saml/" + org + "/metadata" }
func ACSURL(publicURL, org string) string     { return publicURL + "/auth/saml/" + org + "/acs" }

type samlOrg struct {
	id, slug, entityID, ssoURL, groupsAttribute, defaultRole string
	certs                                                    []string
}

func (s *SAML) org(ctx context.Context, slug string) (samlOrg, error) {
	o := samlOrg{slug: slug}
	var id *string
	if err := s.Pool.QueryRow(ctx, `SELECT org_by_slug($1)::text`, slug).Scan(&id); err != nil {
		return o, err
	}
	if id == nil {
		return o, errNoSAML
	}
	o.id = *id
	err := s.Pool.QueryRow(ctx, `SELECT idp_entity_id, sso_url, certificates, groups_attribute, default_role::text FROM org_saml WHERE org_id = $1`,
		o.id).Scan(&o.entityID, &o.ssoURL, &o.certs, &o.groupsAttribute, &o.defaultRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, errNoSAML
	}
	return o, err
}

func (s *SAML) provider(o samlOrg) (*saml2.SAMLServiceProvider, error) {
	store := &dsig.MemoryX509CertificateStore{}
	for _, p := range o.certs {
		block, _ := pem.Decode([]byte(p))
		if block == nil {
			return nil, errors.New("an IdP certificate is not PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		store.Roots = append(store.Roots, cert)
	}
	return &saml2.SAMLServiceProvider{
		IdentityProviderSSOURL:      o.ssoURL,
		IdentityProviderIssuer:      o.entityID,
		ServiceProviderIssuer:       SPEntityID(s.PublicURL, o.slug),
		AssertionConsumerServiceURL: ACSURL(s.PublicURL, o.slug),
		AudienceURI:                 SPEntityID(s.PublicURL, o.slug),
		IDPCertificateStore:         store,
		NameIdFormat:                emailFormat,
		AllowMissingAttributes:      true,
	}, nil
}

func (s *SAML) fail(w http.ResponseWriter, r *http.Request, reason string, err error) {
	slog.Warn("saml sign-in failed", "org", r.PathValue("org"), "reason", reason, "err", err)
	http.Redirect(w, r, "/sign-in?error=saml-"+reason, http.StatusFound)
}

// https://docs.oasis-open.org/security/saml/v2.0/saml-metadata-2.0-os.pdf
func (s *SAML) metadata(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	md := types.EntityDescriptor{EntityID: SPEntityID(s.PublicURL, org), SPSSODescriptor: &types.SPSSODescriptor{
		ProtocolSupportEnumeration: "urn:oasis:names:tc:SAML:2.0:protocol",
		NameIDFormats:              []string{emailFormat}, WantAssertionsSigned: true,
		AssertionConsumerServices: []types.IndexedEndpoint{{
			Binding: "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST", Location: ACSURL(s.PublicURL, org), Index: 1,
		}},
	}}
	b, err := xml.MarshalIndent(md, "", "  ")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, _ = w.Write(append([]byte(xml.Header), b...))
}

// start sends the browser to the IdP with an AuthnRequest (HTTP-Redirect).
func (s *SAML) start(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, err := s.org(ctx, r.PathValue("org"))
	if err != nil {
		s.fail(w, r, "setup", err)
		return
	}
	sp, err := s.provider(o)
	if err != nil {
		s.fail(w, r, "setup", err)
		return
	}
	doc, err := sp.BuildAuthRequestDocument()
	if err != nil {
		s.fail(w, r, "setup", err)
		return
	}
	id := doc.Root().SelectAttrValue("ID", "")
	next := safeNext(r.URL.Query().Get("next"), "/"+o.slug)
	if _, err := s.Pool.Exec(ctx, `DELETE FROM saml_requests WHERE expires_at < now()`); err != nil {
		s.fail(w, r, "internal", err)
		return
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO saml_requests (id, org_id, next, expires_at) VALUES ($1, $2, $3, $4)`,
		id, o.id, next, time.Now().Add(samlRequestLifetime)); err != nil {
		s.fail(w, r, "internal", err)
		return
	}
	// The request is bound to this browser, so a member of the IdP cannot
	// make another person's browser sign in as that member. The IdP posts
	// back from another site, which only a SameSite=None cookie survives,
	// and browsers take one only over https.
	if s.Cookies.Secure {
		http.SetCookie(w, &http.Cookie{Name: samlCookie, Value: id, Path: "/auth/saml/", MaxAge: int(samlRequestLifetime.Seconds()),
			HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode})
	}
	u, err := sp.BuildAuthURLRedirect(id, doc)
	if err != nil {
		s.fail(w, r, "setup", err)
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther) //nolint:gosec // the org's IdP sign-in URL, which its admin set
}

// acs takes the IdP's response (HTTP-POST): its signature, audience and
// times, and that it answers a request this org made.
func (s *SAML) acs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, err := s.org(ctx, r.PathValue("org"))
	if err != nil {
		s.fail(w, r, "setup", err)
		return
	}
	sp, err := s.provider(o)
	if err != nil {
		s.fail(w, r, "setup", err)
		return
	}
	info, err := sp.RetrieveAssertionInfo(r.PostFormValue("SAMLResponse"))
	if err != nil {
		s.fail(w, r, "response", err)
		return
	}
	if info.WarningInfo.InvalidTime || info.WarningInfo.NotInAudience {
		s.fail(w, r, "response", errAssertion)
		return
	}
	inResponseTo := ""
	if sub := info.Assertions[0].Subject; sub != nil && sub.SubjectConfirmation != nil && sub.SubjectConfirmation.SubjectConfirmationData != nil {
		inResponseTo = sub.SubjectConfirmation.SubjectConfirmationData.InResponseTo
	}
	if s.Cookies.Secure {
		c, err := r.Cookie(samlCookie)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(inResponseTo)) != 1 {
			s.fail(w, r, "browser", errOtherBrowser)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: samlCookie, Path: "/auth/saml/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode})
	}
	email := strings.ToLower(strings.TrimSpace(info.NameID))
	if !strings.Contains(email, "@") {
		s.fail(w, r, "email", errNotAnAddress)
		return
	}
	var groups []string
	for _, v := range info.Values[o.groupsAttribute].Values {
		groups = append(groups, strings.TrimSpace(v.Value))
	}
	name := ""
	for _, k := range []string{"displayName", "name", "cn"} {
		if v := info.Values.Get(k); v != "" && name == "" {
			name = v
		}
	}

	var next, token, csrf string
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `DELETE FROM saml_requests WHERE id = $1 AND org_id = $2 AND expires_at > now() RETURNING next`,
			inResponseTo, o.id).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUnsolicited
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, o.id); err != nil {
			return err
		}
		userID, err := samlUser(ctx, tx, o, email, name, groups)
		if err != nil {
			return err
		}
		if token, csrf, err = CreateSession(ctx, tx, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET sso_org_id = $2 WHERE id_hash = $1`, HashToken(token), o.id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_log (org_id, actor_kind, actor_id, action, target, ip)
			VALUES ($1, 'user', $2, 'auth.saml_sign_in', $3, nullif($4, '')::inet)`, o.id, userID, email, ClientIP(r))
		return err
	})
	switch {
	case errors.Is(err, errUnsolicited):
		s.fail(w, r, "expired", err)
		return
	case errors.Is(err, errOutsideOrg):
		s.fail(w, r, "account", err)
		return
	case errors.Is(err, errOwner):
		s.fail(w, r, "owner", err)
		return
	case err != nil:
		s.fail(w, r, "internal", err)
		return
	}
	s.Cookies.Set(w.Header(), token, csrf)
	http.Redirect(w, r, next, http.StatusFound)
}

// samlUser finds the user an org's IdP signed in, or makes them. An
// existing account must be a member of the org or made by its SCIM; a new
// one joins with the highest role its groups map to, or the default.
// tx has the org set.
func samlUser(ctx context.Context, tx pgx.Tx, o samlOrg, email, name string, groups []string) (string, error) {
	var userID string
	var member, owner bool
	err := tx.QueryRow(ctx, `SELECT u.id, EXISTS (SELECT FROM memberships m WHERE m.user_id = u.id AND m.org_id = $2)
		OR EXISTS (SELECT FROM scim_users s WHERE s.user_id = u.id AND s.org_id = $2 AND s.active),
		EXISTS (SELECT FROM memberships m WHERE m.user_id = u.id AND m.org_id = $2 AND m.role = 'owner')
		FROM users u WHERE lower(u.email) = $1`,
		email, o.id).Scan(&userID, &member, &owner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		userID = uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, email, name) VALUES ($1, $2, $3)`, userID, email, ClipName(name)); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	case !member:
		return "", errOutsideOrg
	case owner:
		return "", errOwner
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role)
		SELECT $1, $2, coalesce((SELECT max(role) FROM org_group_roles WHERE org_id = $1 AND group_name = ANY($3)), $4::member_role)
		ON CONFLICT (org_id, user_id) DO NOTHING`, o.id, userID, groups, o.defaultRole); err != nil {
		return "", fmt.Errorf("add the membership: %w", err)
	}
	return userID, nil
}
