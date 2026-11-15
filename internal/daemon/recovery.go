package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/version"
)

// listenSocket opens orchd's socket, owner-only, and returns what removes it.
func listenSocket() (net.Listener, func(), error) {
	sock := paths.SocketPath()
	_ = os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		return nil, nil, err
	}
	closeSocket := func() { _ = os.Remove(sock) }
	// The socket is the only trust boundary: anything that can connect gets the
	// whole RPC surface (Dispatch, Restart, the RAG store). Left to the ambient
	// umask the mode is an accident, so pin it to owner-only rather than relying
	// on the caller's environment.
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = lis.Close()
		closeSocket()
		return nil, nil, fmt.Errorf("secure socket %s: %w", sock, err)
	}
	return lis, closeSocket, nil
}

// maxStreamsPerConn caps the calls one connection has open at once.
const maxStreamsPerConn = 128

// newGRPCServer attributes each connection to its process so operator-only
// RPCs can refuse queued tasks however they reach the socket (callers.go),
// and, with a resolver, tells each call whom it serves (caller_session.go).
func newGRPCServer(res *callerResolver, extra ...grpc.UnaryServerInterceptor) *grpc.Server {
	unary := []grpc.UnaryServerInterceptor{guardQueuedCallers}
	stream := []grpc.StreamServerInterceptor{guardQueuedStreams}
	if res != nil {
		unary, stream = append(unary, res.unary), append(stream, res.stream)
	}
	unary = append(unary, extra...)
	return grpc.NewServer(
		grpc.Creds(peerCredentials{self: os.Getpid(), parent: procstat.Parent, program: procstat.ReadProgram, started: procstat.StartTime,
			app: newAppVerifier(userHome())}),
		// Reason: gRPC keeps each reply until its client reads it, so a
		// client that sends calls and never reads would make orchd hold
		// every reply. orch's clients use a few streams per connection.
		grpc.MaxConcurrentStreams(maxStreamsPerConn),
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
	)
}

// importLegacyConfig copies an older release's config.yaml into orch.db, once.
// A failure is logged, not fatal: orchd then runs on the stored settings and
// the defaults, and tries again at the next start.
func importLegacyConfig(ctx context.Context, store *sql.DB) {
	path := paths.LegacyConfigFile()
	rep, err := config.ImportLegacy(ctx, store, path)
	if err != nil {
		log.Printf("orchd: could not import %s, so it is ignored for now: %v", path, err)
		return
	}
	if !rep.Ran {
		return
	}
	log.Printf("orchd: imported %s into orch.db; orch no longer reads it (imported: %s; invalid, kept at default: %s; unknown: %s)",
		path, list(rep.Imported), list(rep.Invalid), list(rep.Unknown))
}

func list(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return fmt.Sprint(keys)
}

// recoveryRetry is how long orchd serves only its settings before it exits to
// try the full start again. Without it a failure that clears by itself (the
// network down during the first model download) would leave orchd's services
// down until someone restarted it; the service manager used to retry that.
var recoveryRetry = 10 * time.Minute

// serveRecovery serves the settings alone after cause stopped orchd's services
// from starting, so Orch.app can change the setting at fault. Restarting orchd
// (the app's restart, or the Restart RPC, which exits for the service manager
// to start it again) tries the full start with the new settings.
//
// needsEvidence says whether a spaces policy is applied, so that, as on the
// full server, only Orch.app is the operator then (D-71); nil never needs it.
func serveRecovery(ctx context.Context, cfg *configService, cause error, needsEvidence func() bool) error {
	log.Printf("orchd %s could not start its services: %v — serving settings only, so they can be fixed in Orch.app", version.Version, cause)
	cfg.startupError = cause.Error()
	lis, closeSocket, err := listenSocket()
	if err != nil {
		return errors.Join(cause, err)
	}
	defer closeSocket()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var res *callerResolver
	if needsEvidence != nil {
		res = &callerResolver{needsEvidence: needsEvidence}
	}
	gs := newGRPCServer(res)
	orchdv1.RegisterOrchdServer(gs, &recoveryServer{config: cfg, start: time.Now(), restart: stop})
	retry := time.AfterFunc(recoveryRetry, func() {
		log.Printf("orchd: exiting to try the full start again")
		stop()
	})
	defer retry.Stop()
	go func() {
		<-runCtx.Done()
		stopGracefully(gs, gracefulStopTimeout)
	}()
	return gs.Serve(lis)
}

// recoveryServer is orchd when its services could not start: settings, ping
// and restart. Every other RPC is unimplemented.
type recoveryServer struct {
	orchdv1.UnimplementedOrchdServer
	config  *configService
	start   time.Time
	restart func()
}

func (r *recoveryServer) Ping(context.Context, *orchdv1.PingRequest) (*orchdv1.PingResponse, error) {
	return &orchdv1.PingResponse{Version: version.Version, UptimeSeconds: int64(time.Since(r.start).Seconds())}, nil
}

func (r *recoveryServer) GetConfig(ctx context.Context, _ *orchdv1.GetConfigRequest) (*orchdv1.ConfigState, error) {
	return r.config.state(ctx)
}

func (r *recoveryServer) SetConfig(ctx context.Context, req *orchdv1.SetConfigRequest) (*orchdv1.ConfigState, error) {
	return r.config.set(ctx, req)
}

func (r *recoveryServer) RemoveLegacyConfig(ctx context.Context, _ *orchdv1.RemoveLegacyConfigRequest) (*orchdv1.ConfigState, error) {
	return r.config.removeLegacy(ctx)
}

// Restart exits, for the service manager to start orchd again with the
// settings as they are now; there are no services to restart in place.
func (r *recoveryServer) Restart(context.Context, *orchdv1.RestartRequest) (*orchdv1.RestartResponse, error) {
	log.Printf("orchd: restarting to try the full start again")
	r.restart()
	return &orchdv1.RestartResponse{}, nil
}
