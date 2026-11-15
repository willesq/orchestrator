package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/queue"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/workflows"
)

// appOperatorCtx is the operator shown to be Orch.app (D-71), carrying the
// peer the operator-only guards read.
var appOperatorCtx = context.WithValue(operatorCtx, resolvedKey{}, resolvedCaller{kind: callerOperator, operatorBy: operatorApp})

// applyReadPolicy applies readPolicy to s: acme and globex isolated (acme
// reads personal), oss public (org oss-org), personal private.
func applyReadPolicy(t *testing.T, s *server) {
	t.Helper()
	withEnforcement(t, s)
	code, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"acme", "globex", "personal"} {
		if err := os.MkdirAll(filepath.Join(code, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := draft(t, s, readPolicy(code))
	if len(d.GetProblems()) > 0 {
		t.Fatalf("policy problems: %v", d.GetProblems())
	}
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
}

// taskOf is a context resolved to a tracked queued task of space.
func taskOf(space string) context.Context {
	return as(resolvedCaller{kind: callerTask, task: agentrun.LiveRun{TaskID: "task-" + space, Space: space}})
}

// spacedTasks is a task server under readPolicy with one task dispatched
// from each space and one with none, by space ("" for none).
func spacedTasks(t *testing.T) (*server, map[string]string) {
	t.Helper()
	s, _, disp := newTaskServer(t)
	sp := spacesServer(t)
	applyReadPolicy(t, sp)
	s.spaces, s.enforcement = sp.spaces, sp.enforcement
	ids := map[string]string{}
	for _, space := range []string{"acme", "globex", "oss", "personal", spaces.Quarantine, ""} {
		id, err := disp.Dispatch(queue.WithReadSpace(context.Background(), space), "dev", space+" job", "", workflows.Route{})
		if err != nil {
			t.Fatal(err)
		}
		ids[space] = id
	}
	return s, ids
}

// visibleTasks lists the IDs ListTasks gives ctx's caller.
func visibleTasks(t *testing.T, s *server, ctx context.Context) []string {
	t.Helper()
	rep, err := s.ListTasks(ctx, &orchdv1.ListTasksRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tk := range rep.GetTasks() {
		out = append(out, tk.GetId())
	}
	slices.Sort(out)
	return out
}

func idsOf(ids map[string]string, spaces ...string) []string {
	var out []string
	for _, sp := range spaces {
		out = append(out, ids[sp])
	}
	slices.Sort(out)
	return out
}

// TestTaskLedgerKeepsSpacesApart is SP-026.1.a1 for ListTasks, GetTask,
// WatchTasks and GetTaskActivity: a caller sees the tasks of the spaces it
// reads, by the space each recorded at dispatch (D-46); a task with none
// is the operator's alone, and the operator sees every task.
func TestTaskLedgerKeepsSpacesApart(t *testing.T) {
	s, ids := spacedTasks(t)
	for _, tt := range []struct {
		name string
		ctx  context.Context
		want []string
	}{
		{"a session in acme", inSpace("acme"), idsOf(ids, "acme", "oss", "personal")},
		{"a session in globex", inSpace("globex"), idsOf(ids, "globex", "oss")},
		{"a task of globex", taskOf("globex"), idsOf(ids, "globex", "oss")},
		{"an unpinned harness", as(none("no pin")), idsOf(ids, "oss")},
		{"a session pinned to quarantine", inSpace(spaces.Quarantine), idsOf(ids, "oss")},
		{"the operator", appOperatorCtx, idsOf(ids, "acme", "globex", "oss", "personal", spaces.Quarantine, "")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := visibleTasks(t, s, tt.ctx); !slices.Equal(got, tt.want) {
				t.Fatalf("ListTasks = %v, want %v", got, tt.want)
			}
			for space, id := range ids {
				rep, err := s.GetTask(tt.ctx, &orchdv1.GetTaskRequest{Id: id})
				if err != nil {
					t.Fatal(err)
				}
				if want := slices.Contains(tt.want, id); rep.GetFound() != want {
					t.Errorf("GetTask(%q task) found = %v, want %v", space, rep.GetFound(), want)
				}
			}
			stream := &fakeWatchStream{ctx: tt.ctx}
			if err := s.sendTaskSnapshot(tt.ctx, stream, queue.ListFilter{}); err != nil {
				t.Fatal(err)
			}
			if err := s.sendTaskChanges(tt.ctx, stream, "", []string{ids["globex"], ids[""]}); err != nil {
				t.Fatal(err)
			}
			if err := s.sendTaskTick(tt.ctx, stream, ""); err != nil {
				t.Fatal(err)
			}
			for space, id := range ids {
				for _, kind := range []string{"snapshot", "changed", "tick"} {
					if stream.find(kind, id) && !slices.Contains(tt.want, id) {
						t.Errorf("WatchTasks %s sent %q's task", kind, space)
					}
				}
			}
		})
	}
	// Another space's transcript reads as none.
	tr := filepath.Join(s.home, ".claude", "projects", "-repo", agentrun.SessionID(ids["globex"])+".jsonl")
	if err := os.MkdirAll(filepath.Dir(tr), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","timestamp":"2026-11-14T10:00:00Z","message":{"content":[{"type":"text","text":"globex secret"}]}}` + "\n"
	if err := os.WriteFile(tr, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if a, err := s.GetTaskActivity(inSpace("acme"), &orchdv1.GetTaskActivityRequest{Id: ids["globex"]}); err != nil || a.GetAvailable() || len(a.GetSteps()) > 0 {
		t.Errorf("acme read globex's task activity: %+v, %v", a, err)
	}
	if a, err := s.GetTaskActivity(inSpace("globex"), &orchdv1.GetTaskActivityRequest{Id: ids["globex"]}); err != nil || !a.GetAvailable() {
		t.Errorf("globex cannot read its own task's activity: %+v, %v", a, err)
	}
	// With no policy every caller sees every task (INV-10).
	s.spaces = nil
	if got := visibleTasks(t, s, inSpace("acme")); len(got) != len(ids) {
		t.Errorf("with no policy a session sees %d of %d tasks", len(got), len(ids))
	}
}

// TestRetryOnlyFromTheTasksSpace is RC-708: a task is retried, cancelled or
// rated only by a caller acting in the space it recorded, or the operator;
// any other caller is told it does not exist, even for a space it reads.
func TestRetryOnlyFromTheTasksSpace(t *testing.T) {
	s, ids := spacedTasks(t)
	s.rater = refusingRater{}
	for _, tt := range []struct {
		name string
		ctx  context.Context
		id   string
		ok   bool
	}{
		{"acme retries globex's", inSpace("acme"), ids["globex"], false},
		{"acme retries public oss's", inSpace("acme"), ids["oss"], false},
		{"acme retries personal's, which it reads", inSpace("acme"), ids["personal"], false},
		{"acme retries one with no space", inSpace("acme"), ids[""], false},
		{"acme retries its own", inSpace("acme"), ids["acme"], true},
		{"a task of acme retries acme's", taskOf("acme"), ids["acme"], true},
		{"an unpinned harness", as(none("no pin")), ids["oss"], false},
		{"a session pinned to quarantine retries a quarantined task", inSpace(spaces.Quarantine), ids[spaces.Quarantine], false},
		{"the operator retries globex's", appOperatorCtx, ids["globex"], true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for rpc, call := range map[string]func() error{
				"RetryTask": func() error {
					_, err := s.RetryTask(tt.ctx, &orchdv1.RetryTaskRequest{Id: tt.id})
					return err
				},
				"CancelTask": func() error {
					_, err := s.CancelTask(tt.ctx, &orchdv1.CancelTaskRequest{Id: tt.id})
					return err
				},
				"RateTask": func() error {
					_, err := s.RateTask(tt.ctx, &orchdv1.RateTaskRequest{Id: tt.id, Rating: "good"})
					return err
				},
			} {
				err := call()
				if refused := grpcstatus.Code(err) == codes.NotFound; refused == tt.ok {
					t.Errorf("%s = %v; allowed %v", rpc, err, tt.ok)
				}
			}
		})
	}
	// The refusal reads as a task that does not exist.
	_, err := s.RetryTask(inSpace("acme"), &orchdv1.RetryTaskRequest{Id: ids["globex"]})
	_, missing := s.RetryTask(appOperatorCtx, &orchdv1.RetryTaskRequest{Id: "task-none"})
	if grpcstatus.Convert(err).Message() != "no such task: "+ids["globex"] || grpcstatus.Convert(missing).Message() != "no such task: task-none" {
		t.Errorf("refusal %q differs from a missing task's %q", err, missing)
	}
}

// refusingRater rates nothing; a call that reaches it was allowed.
type refusingRater struct{}

func (refusingRater) Rate(context.Context, string, string) error { return nil }

// TestTaskEventsCarryTheTasksSpace is SP-026.1.a1 for events: a finished
// task's event carries the space the task recorded at dispatch, so the hook
// shows it only to sessions that read that space.
func TestTaskEventsCarryTheTasksSpace(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	notifyTaskDone(workflows.AgentTaskInput{ID: "task-g", Agent: "dev", Space: "globex"}, workflows.Outcome{Status: "complete"})
	notifyTaskDone(workflows.AgentTaskInput{ID: "task-n", Agent: "dev"}, workflows.Outcome{Status: "failed"})
	evs, err := events.Read(paths.EventsDir(), taskEventStream)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events = %v, %v", evs, err)
	}
	if evs[0].Space != "globex" || evs[1].Space != "" {
		t.Errorf("event spaces = %q, %q", evs[0].Space, evs[1].Space)
	}
	reads := spaces.SessionReads{Spaces: []string{"acme", "oss"}}
	if reads.Shows(evs[0].Space) || reads.Shows(evs[1].Space) {
		t.Error("acme is shown globex's or an unplaced task's event")
	}
}
