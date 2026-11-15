package daemon

import (
	"context"
	"errors"
	"log"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// The operator unlock's RPCs (SP-077); the registry is unlock.go.

// Reason (SP-077): an unlock makes a terminal the operator, so only the
// user's own terminal asks for one and only Orch.app confirms it.
var _ = registerRPCs(operatorRPC(orchdv1.Orchd_RequestUnlock_FullMethodName, "ask for an operator unlock",
	spaceDecision{spaceOwn, "the caller's own unlock request (SP-077)"}))

// RequestUnlock asks Orch.app to confirm operator scope for the calling
// process and its descendants, and waits for the answer. A confirmed
// request grants nothing until the caller sends the code Orch.app shows
// (ProveUnlock).
func (s *server) RequestUnlock(ctx context.Context, req *orchdv1.RequestUnlockRequest) (*orchdv1.RequestUnlockResponse, error) {
	a, err := unlockRequester(ctx, orchdv1.Orchd_RequestUnlock_FullMethodName)
	if err != nil {
		return nil, err
	}
	secs := req.GetForSeconds()
	if secs < 1 || secs > int64(maxUnlockFor/time.Second) {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "an unlock lasts from 1s to %s", maxUnlockFor)
	}
	if s.unlocks == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "unlocks are unavailable")
	}
	u, err := s.unlocks.request(a, time.Duration(secs)*time.Second)
	switch {
	case errors.Is(err, errUnlockLimit):
		return nil, grpcstatus.Error(codes.ResourceExhausted, err.Error())
	case err != nil:
		return nil, grpcstatus.Error(codes.FailedPrecondition, err.Error())
	}
	log.Printf("orchd: unlock %s requested by process %d for %s; waiting for Orch.app", u.id, a.pid, u.forDur)
	got, err := s.unlocks.wait(ctx, u)
	if err != nil {
		log.Printf("orchd: unlock %s not granted: %s", u.id, grpcstatus.Convert(err).Message())
		return nil, err
	}
	return &orchdv1.RequestUnlockResponse{Unlock: got}, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_ProveUnlock_FullMethodName, "prove an operator unlock",
	spaceDecision{spaceOwn, "the caller's own unlock, without its code (SP-077)"}))

// ProveUnlock grants a confirmed unlock to the process that asked for it,
// once it sends the code Orch.app shows.
func (s *server) ProveUnlock(ctx context.Context, req *orchdv1.ProveUnlockRequest) (*orchdv1.ProveUnlockResponse, error) {
	a, err := unlockRequester(ctx, orchdv1.Orchd_ProveUnlock_FullMethodName)
	if err != nil {
		return nil, err
	}
	if s.unlocks == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "unlocks are unavailable")
	}
	got, err := s.unlocks.prove(a, req.GetId(), req.GetCode())
	if err != nil {
		log.Printf("orchd: unlock %s: a code from process %d was refused: %s", printableID(req.GetId()), a.pid, grpcstatus.Convert(err).Message())
		return nil, err
	}
	log.Printf("orchd: unlock %s granted: process %d and its descendants are the operator until %s",
		got.GetId(), a.pid, time.Unix(got.GetExpiresAt(), 0).Format(time.RFC3339))
	return &orchdv1.ProveUnlockResponse{Unlock: got}, nil
}

// unlockRequester returns ctx's caller when it may ask for (or prove) an
// unlock: a process outside any agent harness that orchd did not start,
// while a spaces policy is applied, and not Orch.app itself.
func unlockRequester(ctx context.Context, method string) (callerAuth, error) {
	if err := refuseQueued(ctx, method); err != nil {
		return callerAuth{}, err
	}
	c := callerOf(ctx)
	a, ok := peerAuth(ctx)
	switch {
	case !ok || c.kind != callerOperator:
		who, _ := operator(ctx)
		logRefusal(method, who)
		return callerAuth{}, grpcstatus.Errorf(codes.PermissionDenied,
			"an unlock can only be asked for from your own terminal, not inside an agent session or a task orchd runs (caller: %s)", who)
	case c.operatorBy == operatorApp:
		return callerAuth{}, grpcstatus.Error(codes.InvalidArgument, "Orch.app is the operator already")
	case !c.evidenceNeeded:
		return callerAuth{}, grpcstatus.Error(codes.FailedPrecondition, "no spaces policy is applied, so this terminal is the operator already")
	}
	return a, nil
}

// peerAuth returns what orchd learned about ctx's connection.
func peerAuth(ctx context.Context) (callerAuth, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return callerAuth{}, false
	}
	a, ok := p.AuthInfo.(callerAuth)
	return a, ok && a.err == nil
}

// logRefusal logs a refused unlock RPC, at most once a second.
func logRefusal(method, who string) {
	if unlockLogs.allow(time.Now()) {
		log.Printf("orchd: refused %s from %s", rpcName(method), who)
	}
}

// refuseNonApp refuses method unless ctx's caller is Orch.app, recognized
// by its code signature (D-71). An unlocked terminal is not enough: it must
// not confirm unlocks of its own.
func refuseNonApp(ctx context.Context, method, action string) error {
	if err := refuseQueued(ctx, method); err != nil {
		return err
	}
	if c := callerOf(ctx); c.kind == callerOperator && c.operatorBy == operatorApp {
		return nil
	}
	who, ok := operator(ctx)
	if ok {
		who = "a process that is not Orch.app"
	}
	logRefusal(method, who)
	return grpcstatus.Errorf(codes.PermissionDenied, "only Orch.app can %s (caller: %s)", action, who)
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_ListUnlocks_FullMethodName, "list operator unlocks",
	spaceDecision{spaceOperator, "pending and active unlocks with their command lines; Orch.app only (SP-077)"}))

// ListUnlocks lists pending and active unlocks for Orch.app. With no policy
// applied there are none, and the operator gets an empty list.
func (s *server) ListUnlocks(ctx context.Context, _ *orchdv1.ListUnlocksRequest) (*orchdv1.ListUnlocksResponse, error) {
	const method = orchdv1.Orchd_ListUnlocks_FullMethodName
	if c := callerOf(ctx); c.kind == callerOperator && !c.evidenceNeeded {
		if err := refuseNonOperator(ctx, method, operatorActions[method]); err != nil {
			return nil, err
		}
		return &orchdv1.ListUnlocksResponse{}, nil
	}
	if err := refuseNonApp(ctx, method, operatorActions[method]); err != nil {
		return nil, err
	}
	if s.unlocks == nil {
		return &orchdv1.ListUnlocksResponse{}, nil
	}
	return &orchdv1.ListUnlocksResponse{Unlocks: s.unlocks.list()}, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_ConfirmUnlock_FullMethodName, "confirm operator unlocks",
	spaceDecision{spaceOperator, "confirms or denies an unlock; Orch.app only (SP-077)"}))

// ConfirmUnlock confirms or denies a pending unlock; Orch.app only.
func (s *server) ConfirmUnlock(ctx context.Context, req *orchdv1.ConfirmUnlockRequest) (*orchdv1.ConfirmUnlockResponse, error) {
	const method = orchdv1.Orchd_ConfirmUnlock_FullMethodName
	if err := refuseNonApp(ctx, method, operatorActions[method]); err != nil {
		return nil, err
	}
	if s.unlocks == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "unlocks are unavailable")
	}
	u, err := s.unlocks.confirm(req.GetId(), req.GetAllow())
	if err != nil {
		return nil, err
	}
	verb := "denied"
	if req.GetAllow() {
		verb = "confirmed"
	}
	log.Printf("orchd: unlock %s %s in Orch.app", printableID(req.GetId()), verb)
	return &orchdv1.ConfirmUnlockResponse{Unlock: u}, nil
}

var _ = registerRPCs(operatorRPC(orchdv1.Orchd_Lock_FullMethodName, "end operator unlocks",
	spaceDecision{spaceNone, "ends unlocks; answers a count (SP-077)"}))

// Lock ends unlocks. Orch.app ends the one named, or every one; a terminal
// ends those covering it (the one named, or all of them).
func (s *server) Lock(ctx context.Context, req *orchdv1.LockRequest) (*orchdv1.LockResponse, error) {
	const method = orchdv1.Orchd_Lock_FullMethodName
	if err := refuseQueued(ctx, method); err != nil {
		return nil, err
	}
	c := callerOf(ctx)
	a, ok := peerAuth(ctx)
	if !ok || c.kind != callerOperator {
		who, _ := operator(ctx)
		logRefusal(method, who)
		return nil, grpcstatus.Errorf(codes.PermissionDenied,
			"only Orch.app or your own terminal can end an unlock, not an agent session or a task orchd runs (caller: %s)", who)
	}
	if s.unlocks == nil {
		return &orchdv1.LockResponse{}, nil
	}
	id := req.GetId()
	var keep func(*unlock) bool
	if c.operatorBy == operatorApp {
		keep = func(u *unlock) bool { return id == "" || u.id == id }
	} else {
		// Reason: a terminal ends only what covers it, so one terminal
		// cannot end another's unlock.
		mine := s.unlocks.covering(a)
		keep = func(u *unlock) bool { return slices.Contains(mine, u) && (id == "" || u.id == id) }
	}
	n := s.unlocks.lock(keep)
	if n > 0 {
		log.Printf("orchd: %d unlock(s) ended by %s", n, readerLabel(c))
	}
	return &orchdv1.LockResponse{Ended: int32(n)}, nil
}
