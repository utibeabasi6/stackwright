package api

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/utibeabasi6/stackwright/internal/auth"
	"github.com/utibeabasi6/stackwright/internal/authz"
	"github.com/utibeabasi6/stackwright/internal/seal"
)

type Instance struct {
	stackwrightv1connect.UnimplementedInstanceServiceHandler
	KEK    seal.KEK
	SignIn *auth.SignIn
}

func kindToDB(k v1.SignInProviderKind) string {
	return map[v1.SignInProviderKind]string{
		v1.SignInProviderKind_SIGN_IN_PROVIDER_KIND_OIDC:   "oidc",
		v1.SignInProviderKind_SIGN_IN_PROVIDER_KIND_GITHUB: "github",
	}[k]
}

func (i *Instance) ListSignInProviderSettings(ctx context.Context, _ *connect.Request[v1.ListSignInProviderSettingsRequest]) (*connect.Response[v1.ListSignInProviderSettingsResponse], error) {
	rows, err := authz.ScopeFrom(ctx).Tx.Query(ctx, `SELECT id, kind, name, issuer, client_id FROM sign_in_providers ORDER BY name`)
	if err != nil {
		return nil, authz.Internal(err)
	}
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*v1.SignInProvider, error) {
		p := &v1.SignInProvider{}
		var kind string
		err := r.Scan(&p.Id, &kind, &p.Name, &p.Issuer, &p.ClientId)
		p.Kind = v1.SignInProviderKind_SIGN_IN_PROVIDER_KIND_OIDC
		if kind == "github" {
			p.Kind = v1.SignInProviderKind_SIGN_IN_PROVIDER_KIND_GITHUB
		}
		p.CallbackUrl = i.SignIn.CallbackURL(p.Id)
		return p, err
	})
	if err != nil {
		return nil, authz.Internal(err)
	}
	return connect.NewResponse(&v1.ListSignInProviderSettingsResponse{Providers: ps}), nil
}

func checkIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return invalid("the issuer must be a URL")
	}
	host := u.Hostname()
	local := host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host).IsLoopback()
	if u.Scheme != "https" && (u.Scheme != "http" || !local) {
		return invalid("the issuer must use https")
	}
	return nil
}

func (i *Instance) CreateSignInProvider(ctx context.Context, req *connect.Request[v1.CreateSignInProviderRequest]) (*connect.Response[v1.CreateSignInProviderResponse], error) {
	s := authz.ScopeFrom(ctx)
	m := req.Msg
	kind := kindToDB(m.Kind)
	name := strings.TrimSpace(m.Name)
	switch {
	case kind == "":
		return nil, invalid("choose OIDC or GitHub")
	case name == "" || len(name) > 40:
		return nil, invalid("the name needs 1 to 40 characters")
	case m.ClientId == "" || m.ClientSecret == "":
		return nil, invalid("the client ID and secret are required")
	}
	issuer := ""
	if kind == "oidc" {
		issuer = strings.TrimRight(m.Issuer, "/")
		if err := checkIssuer(issuer); err != nil {
			return nil, err
		}
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := oidc.NewProvider(dctx, issuer); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("could not read the issuer's OIDC discovery document: "+err.Error()))
		}
	}
	sealed, err := i.KEK.Seal(ctx, s.Tx, "", []byte(m.ClientSecret))
	if err != nil {
		return nil, authz.Internal(err)
	}
	id := uuid.NewString()
	if _, err := s.Tx.Exec(ctx, `INSERT INTO sign_in_providers (id, kind, name, issuer, client_id, client_secret)
		VALUES ($1, $2, $3, $4, $5, $6)`, id, kind, name, issuer, m.ClientId, sealed); err != nil {
		return nil, authz.Internal(err)
	}
	s.Target("sign_in_provider:" + id)
	return connect.NewResponse(&v1.CreateSignInProviderResponse{Provider: &v1.SignInProvider{
		Id: id, Kind: m.Kind, Name: name, Issuer: issuer, ClientId: m.ClientId, CallbackUrl: i.SignIn.CallbackURL(id),
	}}), nil
}

func (i *Instance) DeleteSignInProvider(ctx context.Context, req *connect.Request[v1.DeleteSignInProviderRequest]) (*connect.Response[v1.DeleteSignInProviderResponse], error) {
	s := authz.ScopeFrom(ctx)
	if _, err := uuid.Parse(req.Msg.Id); err != nil {
		return nil, errNotFound
	}
	tag, err := s.Tx.Exec(ctx, `DELETE FROM sign_in_providers WHERE id = $1`, req.Msg.Id)
	if err != nil {
		return nil, authz.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound
	}
	i.SignIn.Forget(req.Msg.Id)
	s.Target("sign_in_provider:" + req.Msg.Id)
	return connect.NewResponse(&v1.DeleteSignInProviderResponse{}), nil
}
