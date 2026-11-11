package api

import (
	"context"

	"connectrpc.com/connect"

	v1 "github.com/stackwright/stackwright/gen/stackwright/v1"
	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/version"
)

type Meta struct {
	stackwrightv1connect.UnimplementedMetaServiceHandler
}

func (Meta) GetVersion(context.Context, *connect.Request[v1.GetVersionRequest]) (*connect.Response[v1.GetVersionResponse], error) {
	return connect.NewResponse(&v1.GetVersionResponse{Version: version.Version}), nil
}
