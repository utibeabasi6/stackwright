// Package scim serves SCIM 2.0 Users and Groups for each org, so an
// identity provider can add and remove members (adr/0045).
// https://www.rfc-editor.org/rfc/rfc7643
// https://www.rfc-editor.org/rfc/rfc7644
package scim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stackwright/stackwright/internal/auth"
)

// TokenPrefix starts an org's SCIM token.
const TokenPrefix = "sws_"

const (
	userSchema  = "urn:ietf:params:scim:schemas:core:2.0:User"
	groupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
	listSchema  = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	errorSchema = "urn:ietf:params:scim:api:messages:2.0:Error"
	maxBody     = 1 << 20
	maxPage     = 200
)

// Server serves /scim/v2/{org}/.
type Server struct {
	Pool      *pgxpool.Pool
	PublicURL string
}

type scimError struct {
	status int
	typ    string
	detail string
}

func (e *scimError) Error() string { return e.detail }

func fail(status int, typ, format string, a ...any) error {
	return &scimError{status: status, typ: typ, detail: fmt.Sprintf(format, a...)}
}

var errNotFound = fail(http.StatusNotFound, "", "not found")

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /scim/v2/{org}/ServiceProviderConfig", s.serve(s.config))
	mux.HandleFunc("GET /scim/v2/{org}/Users", s.serve(s.listUsers))
	mux.HandleFunc("POST /scim/v2/{org}/Users", s.serve(s.createUser))
	mux.HandleFunc("GET /scim/v2/{org}/Users/{id}", s.serve(s.getUser))
	mux.HandleFunc("PUT /scim/v2/{org}/Users/{id}", s.serve(s.putUser))
	mux.HandleFunc("PATCH /scim/v2/{org}/Users/{id}", s.serve(s.patchUser))
	mux.HandleFunc("DELETE /scim/v2/{org}/Users/{id}", s.serve(s.deleteUser))
	mux.HandleFunc("GET /scim/v2/{org}/Groups", s.serve(s.listGroups))
	mux.HandleFunc("POST /scim/v2/{org}/Groups", s.serve(s.createGroup))
	mux.HandleFunc("GET /scim/v2/{org}/Groups/{id}", s.serve(s.getGroup))
	mux.HandleFunc("PUT /scim/v2/{org}/Groups/{id}", s.serve(s.putGroup))
	mux.HandleFunc("PATCH /scim/v2/{org}/Groups/{id}", s.serve(s.patchGroup))
	mux.HandleFunc("DELETE /scim/v2/{org}/Groups/{id}", s.serve(s.deleteGroup))
	return mux
}

// call is one request inside its org's transaction.
type call struct {
	ctx   context.Context
	tx    pgx.Tx
	org   string
	slug  string
	r     *http.Request
	base  string
	audit []string
}

// serve checks the org's bearer token, runs fn in a transaction with the
// org set, and writes its result as SCIM JSON.
func (s *Server) serve(fn func(*call) (int, any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		write := func(status int, v any) {
			w.Header().Set("Content-Type", "application/scim+json")
			w.WriteHeader(status)
			if v != nil {
				_ = json.NewEncoder(w).Encode(v)
			}
		}
		writeErr := func(err error) {
			var se *scimError
			if !errors.As(err, &se) {
				slog.Error("scim", "path", r.URL.Path, "err", err)
				se = &scimError{status: http.StatusInternalServerError, detail: "internal error"}
			}
			body := map[string]any{"schemas": []string{errorSchema}, "status": strconv.Itoa(se.status), "detail": se.detail}
			if se.typ != "" {
				body["scimType"] = se.typ
			}
			write(se.status, body)
		}
		slug := r.PathValue("org")
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var org *string
		if err := s.Pool.QueryRow(r.Context(), `SELECT t.org_id::text FROM scim_tokens t WHERE t.token_hash = $1 AND t.org_id = org_by_slug($2)`,
			auth.HashToken(bearer), slug).Scan(&org); err != nil && !errors.Is(err, pgx.ErrNoRows) || org == nil {
			writeErr(fail(http.StatusUnauthorized, "", "the SCIM token is not this org's"))
			return
		}
		c := &call{ctx: r.Context(), org: *org, slug: slug, r: r, base: s.PublicURL + "/scim/v2/" + slug}
		var status int
		var out any
		err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
			c.tx = tx
			if _, err := tx.Exec(c.ctx, `SELECT set_config('stackwright.org_id', $1, true)`, c.org); err != nil {
				return err
			}
			var err error
			if status, out, err = fn(c); err != nil {
				return err
			}
			for _, a := range c.audit {
				action, target, _ := strings.Cut(a, " ")
				if _, err := tx.Exec(c.ctx, `INSERT INTO audit_log (org_id, actor_kind, actor_id, action, target, ip)
					VALUES ($1, 'system', 'scim', $2, $3, nullif($4, '')::inet)`, c.org, action, target, auth.ClientIP(r)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			writeErr(err)
			return
		}
		write(status, out)
	}
}

func (c *call) decode(v any) error {
	if err := json.NewDecoder(io.LimitReader(c.r.Body, maxBody)).Decode(v); err != nil {
		return fail(http.StatusBadRequest, "invalidSyntax", "the body is not JSON: %v", err)
	}
	return nil
}

func (s *Server) config(c *call) (int, any, error) {
	return http.StatusOK, map[string]any{
		"schemas":        []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":          map[string]bool{"supported": true},
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": maxPage},
		"changePassword": map[string]bool{"supported": false},
		"sort":           map[string]bool{"supported": false},
		"etag":           map[string]bool{"supported": false},
		"authenticationSchemes": []map[string]string{{"type": "oauthbearertoken", "name": "Bearer token",
			"description": "The org's SCIM token from Stackwright's settings"}},
	}, nil
}

// filterEq reads the one filter Stackwright takes: attr eq "value".
var filterEq = regexp.MustCompile(`^\s*(\w+)\s+eq\s+"((?:[^"\\]|\\.)*)"\s*$`)

func (c *call) filter(allowed ...string) (string, string, error) {
	f := c.r.URL.Query().Get("filter")
	if f == "" {
		return "", "", nil
	}
	m := filterEq.FindStringSubmatch(f)
	if m == nil {
		return "", "", fail(http.StatusBadRequest, "invalidFilter", "only `attribute eq \"value\"` filters are supported")
	}
	for _, a := range allowed {
		if strings.EqualFold(m[1], a) {
			return a, strings.ReplaceAll(m[2], `\"`, `"`), nil
		}
	}
	return "", "", fail(http.StatusBadRequest, "invalidFilter", "filter on one of %s", strings.Join(allowed, ", "))
}

func (c *call) page() (int, int) {
	start, _ := strconv.Atoi(c.r.URL.Query().Get("startIndex"))
	count, err := strconv.Atoi(c.r.URL.Query().Get("count"))
	if start < 1 {
		start = 1
	}
	if err != nil || count > maxPage || count < 0 {
		count = maxPage
	}
	return start, count
}

func list(resources []any, total, start int) map[string]any {
	return map[string]any{"schemas": []string{listSchema}, "totalResults": total, "startIndex": start, "itemsPerPage": len(resources),
		"Resources": resources}
}

type email struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
}

// userBody is the part of a SCIM User that Stackwright keeps.
type userBody struct {
	UserName    string  `json:"userName"`
	ExternalID  string  `json:"externalId"`
	DisplayName string  `json:"displayName"`
	Active      *bool   `json:"active"`
	Emails      []email `json:"emails"`
	Name        struct {
		Formatted  string `json:"formatted"`
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"name"`
}

func (b userBody) email() string {
	if strings.Contains(b.UserName, "@") {
		return strings.ToLower(strings.TrimSpace(b.UserName))
	}
	for _, e := range b.Emails {
		if e.Primary || len(b.Emails) == 1 {
			return strings.ToLower(strings.TrimSpace(e.Value))
		}
	}
	return ""
}

func (b userBody) name() string {
	n := b.DisplayName
	if n == "" {
		n = b.Name.Formatted
	}
	if n == "" {
		n = strings.TrimSpace(b.Name.GivenName + " " + b.Name.FamilyName)
	}
	if len(n) > 100 {
		n = n[:100]
	}
	return n
}

type userRow struct {
	id, userID, userName, externalID, email, name string
	active                                        bool
}

func (c *call) userJSON(u userRow) map[string]any {
	return map[string]any{
		"schemas": []string{userSchema}, "id": u.id, "externalId": u.externalID, "userName": u.userName, "displayName": u.name,
		"active": u.active, "emails": []email{{Value: u.email, Primary: true}},
		"meta": map[string]string{"resourceType": "User", "location": c.base + "/Users/" + u.id},
	}
}

const userColumns = `s.id, s.user_id, s.user_name, s.external_id, u.email, u.name, s.active FROM scim_users s JOIN users u ON u.id = s.user_id`

func scanUser(r pgx.CollectableRow) (userRow, error) {
	var u userRow
	return u, r.Scan(&u.id, &u.userID, &u.userName, &u.externalID, &u.email, &u.name, &u.active)
}

func (c *call) user(id string) (userRow, error) {
	if _, err := uuid.Parse(id); err != nil {
		return userRow{}, errNotFound
	}
	rows, err := c.tx.Query(c.ctx, `SELECT `+userColumns+` WHERE s.id = $1`, id)
	if err != nil {
		return userRow{}, err
	}
	u, err := pgx.CollectExactlyOneRow(rows, scanUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, errNotFound
	}
	return u, err
}

func (s *Server) listUsers(c *call) (int, any, error) {
	attr, value, err := c.filter("userName", "externalId")
	if err != nil {
		return 0, nil, err
	}
	q, args := `SELECT `+userColumns+` ORDER BY s.user_name`, []any{}
	switch attr {
	case "userName":
		q, args = `SELECT `+userColumns+` WHERE lower(s.user_name) = lower($1)`, []any{value}
	case "externalId":
		q, args = `SELECT `+userColumns+` WHERE s.external_id = $1`, []any{value}
	}
	start, count := c.page()
	rows, err := c.tx.Query(c.ctx, q, args...)
	if err != nil {
		return 0, nil, err
	}
	all, err := pgx.CollectRows(rows, scanUser)
	if err != nil {
		return 0, nil, err
	}
	var out []any
	for i, u := range all {
		if i >= start-1 && len(out) < count {
			out = append(out, c.userJSON(u))
		}
	}
	return http.StatusOK, list(out, len(all), start), nil
}

func (s *Server) getUser(c *call) (int, any, error) {
	u, err := c.user(c.r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, c.userJSON(u), nil
}

func (s *Server) createUser(c *call) (int, any, error) {
	var b userBody
	if err := c.decode(&b); err != nil {
		return 0, nil, err
	}
	mail := b.email()
	if b.UserName == "" || mail == "" {
		return 0, nil, fail(http.StatusBadRequest, "invalidValue", "a user needs a userName and an email address")
	}
	var exists bool
	if err := c.tx.QueryRow(c.ctx, `SELECT EXISTS (SELECT FROM scim_users WHERE lower(user_name) = lower($1))`, b.UserName).Scan(&exists); err != nil {
		return 0, nil, err
	}
	if exists {
		return 0, nil, fail(http.StatusConflict, "uniqueness", "userName %s exists", b.UserName)
	}
	var userID string
	err := c.tx.QueryRow(c.ctx, `SELECT id FROM users WHERE lower(email) = $1`, mail).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		userID = uuid.NewString()
		_, err = c.tx.Exec(c.ctx, `INSERT INTO users (id, email, name) VALUES ($1, $2, $3)`, userID, mail, b.name())
	}
	if err != nil {
		return 0, nil, err
	}
	id := uuid.NewString()
	active := b.Active == nil || *b.Active
	if _, err := c.tx.Exec(c.ctx, `INSERT INTO scim_users (id, org_id, user_id, user_name, external_id, active) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, c.org, userID, b.UserName, b.ExternalID, active); err != nil {
		return 0, nil, err
	}
	if err := c.sync(id); err != nil {
		return 0, nil, err
	}
	c.audit = append(c.audit, "scim.user_created "+b.UserName)
	u, err := c.user(id)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, c.userJSON(u), nil
}

func (s *Server) putUser(c *call) (int, any, error) {
	u, err := c.user(c.r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	var b userBody
	if err := c.decode(&b); err != nil {
		return 0, nil, err
	}
	active := b.Active == nil || *b.Active
	if _, err := c.tx.Exec(c.ctx, `UPDATE scim_users SET user_name = coalesce(nullif($2, ''), user_name), external_id = $3, active = $4 WHERE id = $1`,
		u.id, b.UserName, b.ExternalID, active); err != nil {
		return 0, nil, err
	}
	if n := b.name(); n != "" {
		if _, err := c.tx.Exec(c.ctx, `UPDATE users SET name = $2 WHERE id = $1`, u.userID, n); err != nil {
			return 0, nil, err
		}
	}
	return c.updated(u.id, "scim.user_updated "+u.userName)
}

// patchOp is one operation of a PATCH; Entra ID sends "Replace" and
// string booleans.
type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func boolOf(raw json.RawMessage) (bool, bool) {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseBool(s); err == nil {
			return v, true
		}
	}
	return false, false
}

func (c *call) ops() ([]patchOp, error) {
	var body struct {
		Operations []patchOp `json:"Operations"`
	}
	if err := c.decode(&body); err != nil {
		return nil, err
	}
	for i := range body.Operations {
		body.Operations[i].Op = strings.ToLower(body.Operations[i].Op)
	}
	return body.Operations, nil
}

func (s *Server) patchUser(c *call) (int, any, error) {
	u, err := c.user(c.r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	ops, err := c.ops()
	if err != nil {
		return 0, nil, err
	}
	for _, op := range ops {
		if op.Op != "replace" && op.Op != "add" {
			continue
		}
		fields := map[string]json.RawMessage{}
		if op.Path == "" {
			if err := json.Unmarshal(op.Value, &fields); err != nil {
				return 0, nil, fail(http.StatusBadRequest, "invalidValue", "a replace with no path takes an object")
			}
		} else {
			fields[op.Path] = op.Value
		}
		for k, v := range fields {
			switch strings.ToLower(k) {
			case "active":
				active, ok := boolOf(v)
				if !ok {
					return 0, nil, fail(http.StatusBadRequest, "invalidValue", "active is true or false")
				}
				if _, err := c.tx.Exec(c.ctx, `UPDATE scim_users SET active = $2 WHERE id = $1`, u.id, active); err != nil {
					return 0, nil, err
				}
			case "externalid":
				var ext string
				_ = json.Unmarshal(v, &ext)
				if _, err := c.tx.Exec(c.ctx, `UPDATE scim_users SET external_id = $2 WHERE id = $1`, u.id, ext); err != nil {
					return 0, nil, err
				}
			case "displayname":
				var n string
				_ = json.Unmarshal(v, &n)
				if len(n) > 100 {
					n = n[:100]
				}
				if _, err := c.tx.Exec(c.ctx, `UPDATE users SET name = $2 WHERE id = $1`, u.userID, n); err != nil {
					return 0, nil, err
				}
			}
		}
	}
	return c.updated(u.id, "scim.user_updated "+u.userName)
}

func (c *call) updated(id, audit string) (int, any, error) {
	if err := c.sync(id); err != nil {
		return 0, nil, err
	}
	c.audit = append(c.audit, audit)
	u, err := c.user(id)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, c.userJSON(u), nil
}

func (s *Server) deleteUser(c *call) (int, any, error) {
	u, err := c.user(c.r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if _, err := c.tx.Exec(c.ctx, `UPDATE scim_users SET active = false WHERE id = $1`, u.id); err != nil {
		return 0, nil, err
	}
	if err := c.sync(u.id); err != nil {
		return 0, nil, err
	}
	if _, err := c.tx.Exec(c.ctx, `DELETE FROM scim_users WHERE id = $1`, u.id); err != nil {
		return 0, nil, err
	}
	c.audit = append(c.audit, "scim.user_deleted "+u.userName)
	return http.StatusNoContent, nil, nil
}

// sync makes a SCIM user's membership match: an active user has the
// highest role of their mapped groups, or the default; an inactive one has
// none, and loses every session and CLI token. An owner is left as is.
func (c *call) sync(scimUserID string) error {
	var userID string
	var active bool
	if err := c.tx.QueryRow(c.ctx, `SELECT user_id, active FROM scim_users WHERE id = $1`, scimUserID).Scan(&userID, &active); err != nil {
		return err
	}
	var owner bool
	if err := c.tx.QueryRow(c.ctx, `SELECT EXISTS (SELECT FROM memberships WHERE org_id = $1 AND user_id = $2 AND role = 'owner')`,
		c.org, userID).Scan(&owner); err != nil {
		return err
	}
	if owner {
		slog.Warn("scim leaves an owner as is", "org", c.slug, "user", userID)
		return nil
	}
	if !active {
		if _, err := c.tx.Exec(c.ctx, `DELETE FROM memberships WHERE org_id = $1 AND user_id = $2`, c.org, userID); err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM sessions WHERE user_id = $1`,
			`UPDATE cli_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`} {
			if _, err := c.tx.Exec(c.ctx, q, userID); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := c.tx.Exec(c.ctx, `INSERT INTO memberships (org_id, user_id, role)
		SELECT $1, $2, coalesce(
			(SELECT max(r.role) FROM org_group_roles r JOIN scim_groups g ON g.org_id = r.org_id AND g.display_name = r.group_name
				JOIN scim_group_members gm ON gm.group_id = g.id WHERE r.org_id = $1 AND gm.scim_user_id = $3),
			(SELECT default_role FROM org_saml WHERE org_id = $1), 'viewer')
		ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role`, c.org, userID, scimUserID)
	return err
}

type member struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
}

type groupBody struct {
	DisplayName string   `json:"displayName"`
	ExternalID  string   `json:"externalId"`
	Members     []member `json:"members"`
}

func (c *call) groupJSON(id string) (map[string]any, error) {
	var name, ext string
	if _, err := uuid.Parse(id); err != nil {
		return nil, errNotFound
	}
	err := c.tx.QueryRow(c.ctx, `SELECT display_name, external_id FROM scim_groups WHERE id = $1`, id).Scan(&name, &ext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := c.tx.Query(c.ctx, `SELECT s.id, s.user_name FROM scim_group_members gm JOIN scim_users s ON s.id = gm.scim_user_id
		WHERE gm.group_id = $1 ORDER BY s.user_name`, id)
	if err != nil {
		return nil, err
	}
	members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (member, error) {
		var m member
		return m, r.Scan(&m.Value, &m.Display)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"schemas": []string{groupSchema}, "id": id, "displayName": name, "externalId": ext, "members": members,
		"meta": map[string]string{"resourceType": "Group", "location": c.base + "/Groups/" + id}}, nil
}

func (s *Server) listGroups(c *call) (int, any, error) {
	attr, value, err := c.filter("displayName", "externalId")
	if err != nil {
		return 0, nil, err
	}
	q := `SELECT id FROM scim_groups ORDER BY display_name`
	args := []any{}
	switch attr {
	case "displayName":
		q, args = `SELECT id FROM scim_groups WHERE display_name = $1`, []any{value}
	case "externalId":
		q, args = `SELECT id FROM scim_groups WHERE external_id = $1`, []any{value}
	}
	rows, err := c.tx.Query(c.ctx, q, args...)
	if err != nil {
		return 0, nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, nil, err
	}
	start, count := c.page()
	var out []any
	for i, id := range ids {
		if i >= start-1 && len(out) < count {
			g, err := c.groupJSON(id)
			if err != nil {
				return 0, nil, err
			}
			out = append(out, g)
		}
	}
	return http.StatusOK, list(out, len(ids), start), nil
}

func (s *Server) getGroup(c *call) (int, any, error) {
	g, err := c.groupJSON(c.r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, g, nil
}

// setMembers adds or removes members of a group, and syncs each.
func (c *call) setMembers(groupID string, ids []string, add bool) error {
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			return fail(http.StatusBadRequest, "invalidValue", "member %q is not a user", id)
		}
		var err error
		if add {
			_, err = c.tx.Exec(c.ctx, `INSERT INTO scim_group_members (group_id, scim_user_id, org_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
				groupID, id, c.org)
		} else {
			_, err = c.tx.Exec(c.ctx, `DELETE FROM scim_group_members WHERE group_id = $1 AND scim_user_id = $2`, groupID, id)
		}
		if err != nil {
			return fail(http.StatusBadRequest, "invalidValue", "member %q is not a user", id)
		}
		if err := c.sync(id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fail(http.StatusBadRequest, "invalidValue", "member %q is not a user", id)
			}
			return err
		}
	}
	return nil
}

func memberIDs(ms []member) []string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.Value
	}
	return ids
}

func (s *Server) createGroup(c *call) (int, any, error) {
	var b groupBody
	if err := c.decode(&b); err != nil {
		return 0, nil, err
	}
	if b.DisplayName == "" {
		return 0, nil, fail(http.StatusBadRequest, "invalidValue", "a group needs a displayName")
	}
	id := uuid.NewString()
	tag, err := c.tx.Exec(c.ctx, `INSERT INTO scim_groups (id, org_id, display_name, external_id) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		id, c.org, b.DisplayName, b.ExternalID)
	if err != nil {
		return 0, nil, err
	}
	if tag.RowsAffected() == 0 {
		return 0, nil, fail(http.StatusConflict, "uniqueness", "group %s exists", b.DisplayName)
	}
	if err := c.setMembers(id, memberIDs(b.Members), true); err != nil {
		return 0, nil, err
	}
	c.audit = append(c.audit, "scim.group_created "+b.DisplayName)
	g, err := c.groupJSON(id)
	return http.StatusCreated, g, err
}

// members lists a group's member ids.
func (c *call) members(groupID string) ([]string, error) {
	rows, err := c.tx.Query(c.ctx, `SELECT scim_user_id::text FROM scim_group_members WHERE group_id = $1`, groupID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Server) putGroup(c *call) (int, any, error) {
	id := c.r.PathValue("id")
	if _, err := c.groupJSON(id); err != nil {
		return 0, nil, err
	}
	var b groupBody
	if err := c.decode(&b); err != nil {
		return 0, nil, err
	}
	if _, err := c.tx.Exec(c.ctx, `UPDATE scim_groups SET display_name = coalesce(nullif($2, ''), display_name), external_id = $3 WHERE id = $1`,
		id, b.DisplayName, b.ExternalID); err != nil {
		return 0, nil, err
	}
	old, err := c.members(id)
	if err != nil {
		return 0, nil, err
	}
	if err := c.setMembers(id, old, false); err != nil {
		return 0, nil, err
	}
	if err := c.setMembers(id, memberIDs(b.Members), true); err != nil {
		return 0, nil, err
	}
	c.audit = append(c.audit, "scim.group_updated "+b.DisplayName)
	g, err := c.groupJSON(id)
	return http.StatusOK, g, err
}

// memberPath reads a removal's path such as members[value eq "id"].
var memberPath = regexp.MustCompile(`^members\[value eq "([^"]+)"\]$`)

func (s *Server) patchGroup(c *call) (int, any, error) {
	id := c.r.PathValue("id")
	g, err := c.groupJSON(id)
	if err != nil {
		return 0, nil, err
	}
	ops, err := c.ops()
	if err != nil {
		return 0, nil, err
	}
	for _, op := range ops {
		var ms []member
		path := strings.TrimSpace(op.Path)
		switch {
		case strings.EqualFold(path, "displayName") && op.Op == "replace":
			var name string
			if err := json.Unmarshal(op.Value, &name); err != nil || name == "" {
				return 0, nil, fail(http.StatusBadRequest, "invalidValue", "displayName is a string")
			}
			if _, err := c.tx.Exec(c.ctx, `UPDATE scim_groups SET display_name = $2 WHERE id = $1`, id, name); err != nil {
				return 0, nil, err
			}
			current, err := c.members(id)
			if err != nil {
				return 0, nil, err
			}
			for _, m := range current {
				if err := c.sync(m); err != nil {
					return 0, nil, err
				}
			}
		case strings.EqualFold(path, "members"):
			if len(op.Value) > 0 {
				if err := json.Unmarshal(op.Value, &ms); err != nil {
					return 0, nil, fail(http.StatusBadRequest, "invalidValue", "members is a list of {value}")
				}
			}
			switch op.Op {
			case "add":
				err = c.setMembers(id, memberIDs(ms), true)
			case "remove":
				if len(ms) == 0 {
					var all []string
					if all, err = c.members(id); err == nil {
						err = c.setMembers(id, all, false)
					}
				} else {
					err = c.setMembers(id, memberIDs(ms), false)
				}
			case "replace":
				var all []string
				if all, err = c.members(id); err == nil {
					if err = c.setMembers(id, all, false); err == nil {
						err = c.setMembers(id, memberIDs(ms), true)
					}
				}
			}
			if err != nil {
				return 0, nil, err
			}
		case op.Op == "remove" && memberPath.MatchString(path):
			if err := c.setMembers(id, []string{memberPath.FindStringSubmatch(path)[1]}, false); err != nil {
				return 0, nil, err
			}
		default:
			return 0, nil, fail(http.StatusBadRequest, "invalidPath", "Stackwright patches displayName and members only")
		}
	}
	c.audit = append(c.audit, fmt.Sprintf("scim.group_updated %v", g["displayName"]))
	out, err := c.groupJSON(id)
	return http.StatusOK, out, err
}

func (s *Server) deleteGroup(c *call) (int, any, error) {
	id := c.r.PathValue("id")
	g, err := c.groupJSON(id)
	if err != nil {
		return 0, nil, err
	}
	members, err := c.members(id)
	if err != nil {
		return 0, nil, err
	}
	if _, err := c.tx.Exec(c.ctx, `DELETE FROM scim_groups WHERE id = $1`, id); err != nil {
		return 0, nil, err
	}
	for _, m := range members {
		if err := c.sync(m); err != nil {
			return 0, nil, err
		}
	}
	c.audit = append(c.audit, fmt.Sprintf("scim.group_deleted %v", g["displayName"]))
	return http.StatusNoContent, nil, nil
}
