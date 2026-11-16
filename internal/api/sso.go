package api

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/russellhaering/gosaml2/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/utibeabasi6/stackwright/internal/auth"
	"github.com/utibeabasi6/stackwright/internal/authz"
	"github.com/utibeabasi6/stackwright/internal/scim"
)

// SSO sets up an org's SAML identity provider and SCIM token (adr/0045).
type SSO struct {
	stackwrightv1connect.UnimplementedSsoServiceHandler
	PublicURL string
	// AllowHTTP lets the server reach plain-HTTP URLs, for tests and
	// development only.
	AllowHTTP bool
}

// checkURL refuses a URL that the server would reach, unless it is HTTPS.
func checkURL(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.Scheme != "https" && (!allowHTTP || u.Scheme != "http") {
		return nil, invalid("the URL must be https://host/…")
	}
	return u, nil
}

const (
	redirectBinding = "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect"
	maxMetadata     = 1 << 20
)

// parseMetadata reads one IdP's entity id, its HTTP-Redirect sign-in URL
// and its signing certificates, as PEM.
// https://docs.oasis-open.org/security/saml/v2.0/saml-metadata-2.0-os.pdf
func parseMetadata(raw []byte, allowHTTP bool) (string, string, []string, error) {
	var ed types.EntityDescriptor
	if err := xml.Unmarshal(raw, &ed); err != nil {
		return "", "", nil, invalid("the metadata is not one IdP's EntityDescriptor: " + err.Error())
	}
	if ed.IDPSSODescriptor == nil || ed.EntityID == "" {
		return "", "", nil, invalid("the metadata has no IDPSSODescriptor")
	}
	sso := ""
	for _, s := range ed.IDPSSODescriptor.SingleSignOnServices {
		if s.Binding == redirectBinding {
			sso = s.Location
		}
	}
	if sso == "" {
		return "", "", nil, invalid("the IdP has no SingleSignOnService with the HTTP-Redirect binding")
	}
	if _, err := checkURL(sso, allowHTTP); err != nil {
		return "", "", nil, invalid("the IdP's sign-in URL must use https")
	}
	var certs []string
	for _, kd := range ed.IDPSSODescriptor.KeyDescriptors {
		if kd.Use != "" && kd.Use != "signing" {
			continue
		}
		for _, c := range kd.KeyInfo.X509Data.X509Certificates {
			der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(c.Data), ""))
			if err != nil {
				return "", "", nil, invalid("an IdP certificate is not base64")
			}
			if _, err := x509.ParseCertificate(der); err != nil {
				return "", "", nil, invalid("an IdP certificate does not parse: " + err.Error())
			}
			certs = append(certs, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
		}
	}
	if len(certs) == 0 {
		return "", "", nil, invalid("the metadata has no signing certificate")
	}
	return ed.EntityID, sso, certs, nil
}

func (h SSO) load(ctx context.Context, tx pgx.Tx, orgID, slug string) (*v1.GetSsoResponse, error) {
	out := &v1.GetSsoResponse{SpEntityId: auth.SPEntityID(h.PublicURL, slug), AcsUrl: auth.ACSURL(h.PublicURL, slug),
		SpMetadataUrl: auth.SPEntityID(h.PublicURL, slug), ScimUrl: h.PublicURL + "/scim/v2/" + slug}
	sso := &v1.Sso{}
	var certs []string
	var role string
	err := tx.QueryRow(ctx, `SELECT metadata_url, idp_entity_id, sso_url, certificates, groups_attribute, default_role::text, required
		FROM org_saml WHERE org_id = $1`, orgID).Scan(&sso.MetadataUrl, &sso.IdpEntityId, &sso.SsoUrl, &certs, &sso.GroupsAttribute, &role, &sso.Required)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, authz.Internal(err)
	default:
		sso.DefaultRole = authz.RoleFromDB(role)
		for _, p := range certs {
			if block, _ := pem.Decode([]byte(p)); block != nil {
				if c, err := x509.ParseCertificate(block.Bytes); err == nil {
					sso.CertificatesExpire = append(sso.CertificatesExpire, timestamppb.New(c.NotAfter))
				}
			}
		}
		rows, err := tx.Query(ctx, `SELECT group_name, role::text FROM org_group_roles WHERE org_id = $1 ORDER BY group_name`, orgID)
		if err != nil {
			return nil, authz.Internal(err)
		}
		if sso.GroupRoles, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.GroupRole, error) {
			g := &v1.GroupRole{}
			var role string
			err := r.Scan(&g.Group, &role)
			g.Role = authz.RoleFromDB(role)
			return g, err
		}); err != nil {
			return nil, authz.Internal(err)
		}
		out.Sso = sso
	}
	var created time.Time
	err = tx.QueryRow(ctx, `SELECT created_at FROM scim_tokens WHERE org_id = $1`, orgID).Scan(&created)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, authz.Internal(err)
	default:
		out.ScimTokenCreatedAt = timestamppb.New(created)
	}
	return out, nil
}

func (h SSO) GetSso(ctx context.Context, req *connect.Request[v1.GetSsoRequest]) (*connect.Response[v1.GetSsoResponse], error) {
	s := authz.ScopeFrom(ctx)
	out, err := h.load(ctx, s.Tx, s.OrgID, req.Msg.Org)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// assignable is a role that an IdP group or the default may give: owner
// is not one, so an IdP cannot make or unmake owners.
func assignable(r v1.Role) bool {
	return authz.ValidRole(r) && r != v1.Role_ROLE_OWNER
}

func (h SSO) SaveSso(ctx context.Context, req *connect.Request[v1.SaveSsoRequest]) (*connect.Response[v1.SaveSsoResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	raw := []byte(m.MetadataXml)
	if (m.MetadataUrl == "") == (m.MetadataXml == "") {
		return nil, invalid("give the IdP's metadata URL or its XML")
	}
	if m.MetadataUrl != "" {
		u, err := checkURL(m.MetadataUrl, h.AllowHTTP)
		if err != nil {
			return nil, err
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, invalid("the metadata URL: " + err.Error())
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fetch the metadata: %w", err))
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fetch the metadata: %s", resp.Status))
		}
		if raw, err = io.ReadAll(io.LimitReader(resp.Body, maxMetadata)); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("fetch the metadata: %w", err))
		}
	}
	entity, sso, certs, err := parseMetadata(raw, h.AllowHTTP)
	if err != nil {
		return nil, err
	}
	attr := strings.TrimSpace(m.GroupsAttribute)
	if attr == "" {
		attr = "groups"
	}
	if !assignable(m.DefaultRole) {
		return nil, invalid("the default role is viewer up to admin")
	}
	seen := map[string]bool{}
	for _, g := range m.GroupRoles {
		if g.Group = strings.TrimSpace(g.Group); g.Group == "" || len(g.Group) > 200 || seen[g.Group] || !assignable(g.Role) {
			return nil, invalid("each group is named once, and maps to a role from viewer up to admin")
		}
		seen[g.Group] = true
	}
	if _, err := s.Tx.Exec(ctx, `INSERT INTO org_saml (org_id, metadata_url, idp_entity_id, sso_url, certificates, groups_attribute, default_role, required)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (org_id) DO UPDATE SET metadata_url = $2, idp_entity_id = $3, sso_url = $4,
		certificates = $5, groups_attribute = $6, default_role = $7, required = $8, updated_at = now()`,
		s.OrgID, m.MetadataUrl, entity, sso, certs, attr, authz.RoleToDB(m.DefaultRole), m.Required); err != nil {
		return nil, authz.Internal(err)
	}
	if _, err := s.Tx.Exec(ctx, `DELETE FROM org_group_roles WHERE org_id = $1`, s.OrgID); err != nil {
		return nil, authz.Internal(err)
	}
	for _, g := range m.GroupRoles {
		if _, err := s.Tx.Exec(ctx, `INSERT INTO org_group_roles (org_id, group_name, role) VALUES ($1, $2, $3)`, s.OrgID, g.Group,
			authz.RoleToDB(g.Role)); err != nil {
			return nil, authz.Internal(err)
		}
	}
	s.Target("sso " + entity)
	out, err := h.load(ctx, s.Tx, s.OrgID, m.Org)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.SaveSsoResponse{Sso: out}), nil
}

func (h SSO) DeleteSso(ctx context.Context, req *connect.Request[v1.DeleteSsoRequest]) (*connect.Response[v1.DeleteSsoResponse], error) {
	s := authz.ScopeFrom(ctx)
	for _, q := range []string{`DELETE FROM org_group_roles WHERE org_id = $1`, `DELETE FROM org_saml WHERE org_id = $1`,
		`DELETE FROM sessions WHERE sso_org_id = $1`,
		`UPDATE cli_tokens SET revoked_at = now() WHERE sso_org_id = $1 AND revoked_at IS NULL`} {
		if _, err := s.Tx.Exec(ctx, q, s.OrgID); err != nil {
			return nil, authz.Internal(err)
		}
	}
	s.Target("sso")
	return connect.NewResponse(&v1.DeleteSsoResponse{}), nil
}

func (h SSO) RotateScimToken(ctx context.Context, _ *connect.Request[v1.RotateScimTokenRequest]) (*connect.Response[v1.RotateScimTokenResponse], error) {
	s := authz.ScopeFrom(ctx)
	token := scim.TokenPrefix + auth.RandomToken()
	if _, err := s.Tx.Exec(ctx, `INSERT INTO scim_tokens (org_id, token_hash) VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET token_hash = $2, created_at = now()`, s.OrgID, auth.HashToken(token)); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("scim token")
	return connect.NewResponse(&v1.RotateScimTokenResponse{Token: token}), nil
}
