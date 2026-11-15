package daemon

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/config"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// setSpacesSwitch changes spaces.enabled through SetConfig. Only the
// operator may (with a policy applied, Orch.app alone), confirmed, and on
// its own, so the change is one logged step; orchd applies it at once. c.mu
// is held.
func (c *configService) setSpacesSwitch(ctx context.Context, values map[string]any, confirm bool) (*orchdv1.ConfigState, error) {
	if len(values) != 1 {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "change %s on its own", config.KeySpacesEnabled)
	}
	if err := refuseNonOperator(ctx, orchdv1.Orchd_SetConfig_FullMethodName, "switch memory spaces on or off"); err != nil {
		return nil, err
	}
	if !confirm {
		return nil, grpcstatus.Error(codes.FailedPrecondition, config.ErrConfirmSensitive.Error()+": "+config.SpacesSwitchHelp)
	}
	if c.spaces == nil {
		return nil, grpcstatus.Error(codes.FailedPrecondition,
			"orchd is serving only its settings; memory spaces can be switched once it starts fully")
	}
	enabled, _ := values[config.KeySpacesEnabled].(bool)
	who, _ := operator(ctx)
	err := c.spaces.change(enabled, who, func() error {
		return config.ApplyStore(ctx, c.db, values, true, "operator")
	})
	if err != nil {
		code := codes.Internal
		if errors.Is(err, config.ErrConfirmSensitive) {
			code = codes.FailedPrecondition
		}
		return nil, grpcstatus.Error(code, err.Error())
	}
	return c.state(ctx)
}
