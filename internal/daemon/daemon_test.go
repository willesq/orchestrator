package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/supervisor"
)

type fakeRag struct {
	chunks int
	hits   []rag.Hit
	err    error
}

func (f fakeRag) IngestText(context.Context, string, rag.Meta) (int, error) {
	return f.chunks, f.err
}

func (f fakeRag) Search(context.Context, string, rag.SearchOpts) ([]rag.Hit, error) {
	return f.hits, f.err
}

type fakeDispatcher struct {
	id  string
	err error
}

func (f fakeDispatcher) Dispatch(context.Context, string, string) (string, error) {
	return f.id, f.err
}

// fakeSupervisor stands in for the real supervisor in the RPC handler tests.
type fakeSupervisor struct {
	statuses   []supervisor.Status
	names      []string
	restartErr error
	allErr     error
	restarted  []string
	restartAll int
}

func (f *fakeSupervisor) Status() []supervisor.Status { return f.statuses }
func (f *fakeSupervisor) Names() []string             { return f.names }

func (f *fakeSupervisor) Restart(name string) error {
	f.restarted = append(f.restarted, name)
	return f.restartErr
}

func (f *fakeSupervisor) RestartAll() error {
	f.restartAll++
	return f.allErr
}

func TestServerIngest(t *testing.T) {
	tests := []struct {
		name       string
		rag        fakeRag
		wantChunks int32
		wantErr    bool
	}{
		{name: "success", rag: fakeRag{chunks: 3}, wantChunks: 3},
		{name: "error propagates", rag: fakeRag{err: errors.New("boom")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: tt.rag}
			resp, err := s.Ingest(context.Background(), &orchdv1.IngestRequest{Text: "x", SourceUri: "u"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && resp.GetChunks() != tt.wantChunks {
				t.Errorf("chunks = %d, want %d", resp.GetChunks(), tt.wantChunks)
			}
		})
	}
}

func TestServerSearch(t *testing.T) {
	tests := []struct {
		name     string
		rag      fakeRag
		wantHits int
		wantErr  bool
	}{
		{name: "maps hits", rag: fakeRag{hits: []rag.Hit{{Text: "t", SourceURI: "u", Tags: []string{"go"}}}}, wantHits: 1},
		{name: "empty", rag: fakeRag{}, wantHits: 0},
		{name: "error", rag: fakeRag{err: errors.New("x")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: tt.rag}
			resp, err := s.Search(context.Background(), &orchdv1.SearchRequest{Query: "q"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(resp.GetHits()) != tt.wantHits {
				t.Errorf("hits = %d, want %d", len(resp.GetHits()), tt.wantHits)
			}
		})
	}
}

func TestServerDispatch(t *testing.T) {
	tests := []struct {
		name    string
		disp    fakeDispatcher
		wantID  string
		wantErr bool
	}{
		{name: "success", disp: fakeDispatcher{id: "task-1"}, wantID: "task-1"},
		{name: "error propagates", disp: fakeDispatcher{err: errors.New("boom")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: fakeRag{}, disp: tt.disp}
			resp, err := s.Dispatch(context.Background(), &orchdv1.DispatchRequest{Body: "x"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && resp.GetInstanceId() != tt.wantID {
				t.Errorf("id = %q, want %q", resp.GetInstanceId(), tt.wantID)
			}
		})
	}
}

func TestServerPing(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "reports version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: fakeRag{}}
			resp, err := s.Ping(context.Background(), &orchdv1.PingRequest{})
			if err != nil {
				t.Fatalf("Ping: %v", err)
			}
			if resp.GetVersion() == "" {
				t.Errorf("empty version")
			}
		})
	}
}

func TestServerStatus(t *testing.T) {
	started := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		sup           supervisorAPI
		wantSubsystem int
		wantState     string
		wantStarted   int64
		wantLastBeat  int64
	}{
		{
			name: "reports each supervised subsystem",
			sup: &fakeSupervisor{statuses: []supervisor.Status{{
				Name: UnitSources, Detail: "freshness", State: supervisor.StateRunning,
				StartedAt: started, LastBeat: started, Restarts: 2,
			}}},
			wantSubsystem: 1,
			wantState:     supervisor.StateRunning,
			wantStarted:   started.Unix(),
			wantLastBeat:  started.Unix(),
		},
		{
			name: "never-started unit reports zero timestamps",
			sup: &fakeSupervisor{statuses: []supervisor.Status{{
				Name: UnitDocs, State: supervisor.StateStopped,
			}}},
			wantSubsystem: 1,
			wantState:     supervisor.StateStopped,
		},
		{
			name:          "no supervisor still reports version and uptime",
			sup:           nil,
			wantSubsystem: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now().Add(-90 * time.Second), sup: tt.sup}
			resp, err := s.Status(context.Background(), &orchdv1.StatusRequest{})
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if resp.GetVersion() == "" {
				t.Errorf("empty version")
			}
			if resp.GetUptimeSeconds() < 89 {
				t.Errorf("uptime = %d, want >= 89", resp.GetUptimeSeconds())
			}
			if len(resp.GetSubsystems()) != tt.wantSubsystem {
				t.Fatalf("got %d subsystems, want %d", len(resp.GetSubsystems()), tt.wantSubsystem)
			}
			if tt.wantSubsystem == 0 {
				return
			}
			sub := resp.GetSubsystems()[0]
			if sub.GetState() != tt.wantState {
				t.Errorf("state = %q, want %q", sub.GetState(), tt.wantState)
			}
			if sub.GetStartedAt() != tt.wantStarted || sub.GetLastBeat() != tt.wantLastBeat {
				t.Errorf("startedAt/lastBeat = %d/%d, want %d/%d",
					sub.GetStartedAt(), sub.GetLastBeat(), tt.wantStarted, tt.wantLastBeat)
			}
		})
	}
}

func TestServerRestart(t *testing.T) {
	tests := []struct {
		name          string
		sup           supervisorAPI
		req           string
		wantCode      codes.Code
		wantRestarted []string
		wantAllCalls  int
		wantErrHas    string
	}{
		{
			name:          "named subsystem restarts in place",
			sup:           &fakeSupervisor{names: []string{UnitSources, UnitDocs}},
			req:           UnitSources,
			wantRestarted: []string{UnitSources},
		},
		{
			name:          "empty name restarts every subsystem",
			sup:           &fakeSupervisor{names: []string{UnitSources, UnitDocs}},
			req:           "",
			wantRestarted: []string{UnitSources, UnitDocs},
			wantAllCalls:  1,
		},
		{
			name:       "unknown name is NOT_FOUND and lists the known ones",
			sup:        &fakeSupervisor{names: []string{UnitSources}, restartErr: fmt.Errorf("%w: %q", supervisor.ErrUnknownUnit, "nope")},
			req:        "nope",
			wantCode:   codes.NotFound,
			wantErrHas: "known subsystems: " + UnitSources,
		},
		{
			name:       "a failing restart is INTERNAL",
			sup:        &fakeSupervisor{names: []string{UnitSources}, restartErr: errors.New("wedged")},
			req:        UnitSources,
			wantCode:   codes.Internal,
			wantErrHas: "wedged",
		},
		{
			name:       "a failing restart-all is INTERNAL",
			sup:        &fakeSupervisor{names: []string{UnitSources}, allErr: errors.New("wedged")},
			req:        "",
			wantCode:   codes.Internal,
			wantErrHas: "wedged",
		},
		{
			name:       "no supervisor is UNAVAILABLE",
			sup:        nil,
			req:        UnitSources,
			wantCode:   codes.Unavailable,
			wantErrHas: "without supervised subsystems",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), sup: tt.sup}
			resp, err := s.Restart(context.Background(), &orchdv1.RestartRequest{Name: tt.req})
			if tt.wantCode != codes.OK {
				if got := grpcstatus.Code(err); got != tt.wantCode {
					t.Fatalf("code = %v, want %v (err %v)", got, tt.wantCode, err)
				}
				if !strings.Contains(err.Error(), tt.wantErrHas) {
					t.Errorf("error %q missing %q", err, tt.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("Restart: %v", err)
			}
			if strings.Join(resp.GetRestarted(), ",") != strings.Join(tt.wantRestarted, ",") {
				t.Errorf("restarted = %v, want %v", resp.GetRestarted(), tt.wantRestarted)
			}
			if got := tt.sup.(*fakeSupervisor).restartAll; got != tt.wantAllCalls {
				t.Errorf("RestartAll called %d times, want %d", got, tt.wantAllCalls)
			}
		})
	}
}
