package daemon

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// While memory spaces are off, the spaces RPCs that change what is enforced
// are refused with a message saying how to turn them on, and the calls the
// tool and session hooks of an earlier install still make are accepted and
// dropped, as orchd before spaces had nothing to do with them. Read-only
// spaces RPCs still answer from the records kept.

// spacesOffRefused change what spaces enforce; they wait for spaces to be on
// and enforcing (not while the store is still being split back).
var spacesOffRefused = map[string]bool{
	orchdv1.Orchd_ApplySpaces_FullMethodName:        true,
	orchdv1.Orchd_SetEnforcement_FullMethodName:     true,
	orchdv1.Orchd_SetSandboxSetting_FullMethodName:  true,
	orchdv1.Orchd_ChangeOutboundRule_FullMethodName: true,
	orchdv1.Orchd_Relabel_FullMethodName:            true,
	orchdv1.Orchd_PurgeQuarantine_FullMethodName:    true,
	orchdv1.Orchd_RetagSource_FullMethodName:        true,
}

// spacesOffDropped are the hooks' calls, answered without effect while
// spaces are switched off.
var spacesOffDropped = map[string]func() any{
	orchdv1.Orchd_RecordToolEvent_FullMethodName: func() any { return &orchdv1.RecordToolEventResponse{} },
	orchdv1.Orchd_PinSession_FullMethodName:      func() any { return &orchdv1.SessionPin{} },
	orchdv1.Orchd_EndSession_FullMethodName:      func() any { return &orchdv1.EndSessionResponse{} },
}

// spacesOffGate is the interceptor applying the lists above.
func spacesOffGate(w *spacesSwitch) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if w == nil {
			return handler(ctx, req)
		}
		if drop, ok := spacesOffDropped[info.FullMethod]; ok && w.wantsOff() {
			return drop(), nil
		}
		if spacesOffRefused[info.FullMethod] && w.off() {
			return nil, grpcstatus.Error(codes.FailedPrecondition, spaces.ErrSpacesOff.Error())
		}
		return handler(ctx, req)
	}
}
