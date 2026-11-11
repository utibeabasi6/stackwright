package meta

import (
	"context"

	"connectrpc.com/connect"

	metav1 "github.com/stackwright/stackwright/gen/stackwright/meta/v1"
	"github.com/stackwright/stackwright/gen/stackwright/meta/v1/metav1connect"
	"github.com/stackwright/stackwright/internal/version"
)

type Service struct {
	metav1connect.UnimplementedMetaServiceHandler
}

func (Service) GetVersion(context.Context, *connect.Request[metav1.GetVersionRequest]) (*connect.Response[metav1.GetVersionResponse], error) {
	return connect.NewResponse(&metav1.GetVersionResponse{Version: version.Version}), nil
}
