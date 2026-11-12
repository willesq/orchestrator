package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// replayRig is a server with an applied policy (the plan's layout under
// code), a session sess-acme pinned to acme, a command log and the guard
// switched to log, with the guard's verdict recorded on every event as
// SP-041 records it.
type replayRig struct {
	s                       *server
	code, acme, globex, own string
}

func replayPolicy(code string, globexKind spaces.Kind) string {
	p := spaces.Policy{Spaces: []spaces.Space{
		{Name: "oss", Kind: spaces.Public, Roots: []string{code}},
		{Name: "personal", Kind: spaces.Private, Roots: []string{code + "/willesq"}},
		{Name: "acme", Kind: spaces.Isolated, GitHubOrgs: []string{"acme"}, Roots: []string{code + "/company1"}, Reads: []string{"personal"}},
		{Name: "globex", Kind: globexKind, GitHubOrgs: []string{"globex"}, Roots: []string{code + "/globex"}},
	}, PublicRootSessions: "personal"}
	b, _ := json.Marshal(p)
	return string(b)
}

func newReplayRig(t *testing.T) replayRig {
	t.Helper()
	s := spacesServer(t)
	code, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := replayRig{s: s, code: code, acme: code + "/company1/app", globex: code + "/globex/app", own: code + "/willesq/notes"}
	for _, d := range []string{r.acme, r.globex, r.own} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := draft(t, s, replayPolicy(code, spaces.Isolated))
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	withPins(t, s, r.acme, code)
	if pin, err := s.PinSession(inClaude, pinReq("sess-acme", r.acme, "")); err != nil || pin.GetSpace() != "acme" {
		t.Fatalf("pin = %v, %v", pin, err)
	}
	withToolLog(t, s)
	withEnforcement(t, s)
	setSwitch(t, s, string(spaces.LayerGuard), spaces.SwitchLog, 0)
	s.guardVerdict = func(rec toollog.Record) (string, string) {
		res, err := s.spaces.Current()
		if err != nil {
			return "", ""
		}
		pol, err := spaces.NewGuardPolicy(res, nil)
		if err != nil {
			return "", ""
		}
		ev := spaces.GuardEventOf(rec.Event, rec.Tool, rec.Cwd, rec.OldCwd, rec.NewCwd, rec.Targets)
		d := spaces.Decide(pol, spaces.GuardPin{Found: true, Space: "acme"}, spaces.GuardEnforcement{Guard: spaces.GuardLog}, ev)
		return string(d.Would), string(d.Rule)
	}
	return r
}

// record sends the session's calls through RecordToolEvent, plus a move
// into globex and a call the guard refused, and returns how many events
// the log holds.
func (r replayRig) record(t *testing.T) int {
	t.Helper()
	ctx := pinnedCtx(t, r.s, "sess-acme")
	cmds := []string{
		"ls",
		"cat " + r.globex + "/secret.txt",
		"gh pr list -R globex/app",
		"grep -r password " + r.code,
		"cat " + r.own + "/todo.md",
	}
	for _, c := range cmds {
		if _, err := r.s.RecordToolEvent(ctx, bashReq("sess-acme", r.acme, c)); err != nil {
			t.Fatal(err)
		}
	}
	move := &orchdv1.RecordToolEventRequest{Event: "cwd-changed", SessionId: "sess-acme", Cwd: r.globex, OldCwd: r.acme, NewCwd: r.globex}
	if _, err := r.s.RecordToolEvent(ctx, move); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, r.s)
	// A call an enforcing guard refused, stored as SP-041 stores it.
	targets, _ := json.Marshal(spaces.ToolTargets{Paths: []string{r.globex + "/x"}})
	logFixture(t, r.s, sessionOwner("sess-acme"), time.Now(), toollog.Record{
		Event: toollog.EventPreToolUse, Tool: "Read", Cwd: r.acme, Targets: string(targets),
		Decision: toollog.DecisionRefused, Rule: string(spaces.RuleOtherJobPath), PolicyRev: 1,
	})
	flushToolLog(t, r.s)
	recs := records(t, r.s, sessionOwner("sess-acme"))
	for _, rec := range recs[:len(cmds)+1] {
		if !strings.HasPrefix(rec.Rule, "would ") || rec.PolicyRev != 1 {
			t.Fatalf("event not recorded with a verdict: %+v", rec)
		}
	}
	return len(recs)
}

func replay(t *testing.T, s *server, req *orchdv1.ReplaySpacesRequest) *orchdv1.ReplaySpacesResponse {
	t.Helper()
	rep, err := s.ReplaySpaces(operatorCtx, req)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// TestReplaySpacesReproducesRecording is SP-047.1.a1 end to end: a session's
// command log, recorded with the guard's verdicts, replays under the
// recording policy with 0 mismatches.
func TestReplaySpacesReproducesRecording(t *testing.T) {
	r := newReplayRig(t)
	n := r.record(t)
	rep := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", All: true})
	if rep.GetEvents() != int64(n) || rep.GetCompared() != int64(n) || rep.GetChanged() != 0 || rep.GetProblems() != 0 || rep.GetUnrecorded() != 0 {
		t.Fatalf("replay = %v", rep)
	}
	if rep.GetMode() != spaces.SwitchLog || !rep.GetPinned() || rep.GetPinSpace() != "acme" || rep.GetOwner() != "sess-acme" {
		t.Errorf("replay header = %v", rep)
	}
	rules := map[string]int{}
	for _, row := range rep.GetRows() {
		if row.GetChanged() || row.GetRecordedVerdict() != row.GetVerdict() || row.GetRecordedRule() != row.GetRule() || row.GetPolicyRevision() != 1 {
			t.Errorf("row %v differs from its recording", row)
		}
		rules[row.GetRule()]++
	}
	t.Logf("%d events replayed, 0 mismatches; rules %v", n, rules)
	for _, want := range []spaces.GuardRule{spaces.RuleWithinSpace, spaces.RuleOtherJobPath, spaces.RuleOtherJobRepo, spaces.RuleOtherJobAncestor} {
		if rules[string(want)] == 0 {
			t.Errorf("no replayed event has rule %s", want)
		}
	}
	if quiet := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme"}); len(quiet.GetRows()) != 0 {
		t.Errorf("without all, unchanged rows were listed: %v", quiet.GetRows())
	}
}

// TestReplaySpacesChangedPolicy is SP-047.1.a2 end to end: under a policy
// in which globex is public, given as a document or as a saved draft
// revision, each call whose verdict changes is listed with its rule id.
func TestReplaySpacesChangedPolicy(t *testing.T) {
	r := newReplayRig(t)
	r.record(t)
	doc := replayPolicy(r.code, spaces.Public)
	rev := draft(t, r.s, doc).GetRevision()
	for name, req := range map[string]*orchdv1.ReplaySpacesRequest{
		"document": {Owner: "sess-acme", PolicyJson: doc},
		"revision": {Owner: "sess-acme", PolicyRevision: rev},
	} {
		t.Run(name, func(t *testing.T) {
			rep := replay(t, r.s, req)
			var got []string
			for _, row := range rep.GetRows() {
				if !row.GetChanged() || row.GetRule() == "" {
					t.Errorf("listed row %v is not a changed verdict with a rule", row)
				}
				got = append(got, fmt.Sprintf("%d %s %s (%s) -> %s (%s)", row.GetSeq(), row.GetTool(),
					row.GetRecordedVerdict(), row.GetRecordedRule(), row.GetVerdict(), row.GetRule()))
			}
			want := []string{
				"2 Bash refuse (other_job_path) -> allow (within_space)",
				"3 Bash refuse (other_job_repo) -> allow (within_space)",
				"4 Bash refuse (other_job_ancestor) -> allow (within_space)",
				"6  refuse (other_job_path) -> allow (within_space)",
				"7 Read refuse (other_job_path) -> allow (within_space)",
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") || rep.GetChanged() != int64(len(want)) {
				t.Errorf("changes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	strict := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", Mode: spaces.SwitchStrict})
	if strict.GetMode() != spaces.SwitchStrict || strict.GetChanged() != 0 {
		t.Errorf("strict replay = %v", strict)
	}
}

func TestReplaySpacesRequests(t *testing.T) {
	r := newReplayRig(t)
	r.record(t)
	for name, req := range map[string]*orchdv1.ReplaySpacesRequest{
		"no owner":         {},
		"bad owner":        {Owner: "a b"},
		"two policies":     {Owner: "sess-acme", PolicyRevision: 1, PolicyJson: "{}"},
		"negative":         {Owner: "sess-acme", PolicyRevision: -1},
		"bad mode":         {Owner: "sess-acme", Mode: "lenient"},
		"bad document":     {Owner: "sess-acme", PolicyJson: "{"},
		"invalid document": {Owner: "sess-acme", PolicyJson: `{"spaces":[{"name":"a","kind":"weird"}]}`},
	} {
		if _, err := r.s.ReplaySpaces(operatorCtx, req); grpcstatus.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.s.ReplaySpaces(operatorCtx, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", PolicyRevision: 9}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("missing revision: %v", err)
	}
	if _, err := r.s.ReplaySpaces(operatorCtx, &orchdv1.ReplaySpacesRequest{Owner: "process:99999"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("unknown process chain: %v", err)
	}
	// A task holds no pin, so the guard refuses its calls as pin_missing.
	logFixture(t, r.s, taskOwner("t-1"), time.Now(), toollog.Record{Event: toollog.EventPreToolUse, Tool: "Bash", Cwd: r.acme, PolicyRev: 1})
	flushToolLog(t, r.s)
	task := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "task:t-1", All: true})
	if task.GetPinned() || task.GetUnrecorded() != 1 || task.GetRows()[0].GetRule() != string(spaces.RulePinMissing) {
		t.Errorf("task replay = %v", task)
	}
}

func TestReplaySpacesOperatorOnly(t *testing.T) {
	r := newReplayRig(t)
	if taskCallable[orchdv1.Orchd_ReplaySpaces_FullMethodName] || operatorActions[orchdv1.Orchd_ReplaySpaces_FullMethodName] == "" {
		t.Error("ReplaySpaces is not classed operator-only")
	}
	for _, ctx := range []context.Context{
		pinnedCtx(t, r.s, "sess-acme"),
		inClaude,
		callerCtx(callerAuth{pid: 7, spawned: true}),
		context.Background(),
	} {
		if _, err := r.s.ReplaySpaces(ctx, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme"}); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Errorf("ReplaySpaces = %v, want PermissionDenied", err)
		}
	}
	none := &server{}
	if _, err := none.ReplaySpaces(operatorCtx, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme"}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("no log: %v", err)
	}
}

// TestReplaySpacesTamperedRevision: an event whose recording revision was
// edited in orch.db is reported as not decided, never judged under it.
func TestReplaySpacesTamperedRevision(t *testing.T) {
	r := newReplayRig(t)
	n := r.record(t)
	if _, err := replayDB(t).Exec(`UPDATE spaces_revisions SET doc = '{"spaces":[]}' WHERE revision = 1`); err != nil {
		t.Fatal(err)
	}
	rep := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme"})
	if rep.GetProblems() != int64(n) || len(rep.GetRows()) != n || !strings.Contains(rep.GetRows()[0].GetProblem(), "changed outside orchd") {
		t.Fatalf("replay = %v", rep)
	}
	if _, err := r.s.ReplaySpaces(operatorCtx, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", PolicyRevision: 1}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("fixed tampered revision: %v", err)
	}
}

func TestReplaySpacesCancelled(t *testing.T) {
	r := newReplayRig(t)
	r.record(t)
	ctx, cancel := context.WithCancel(operatorCtx)
	cancel()
	if _, err := r.s.ReplaySpaces(ctx, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme"}); grpcstatus.Code(err) != codes.Canceled {
		t.Errorf("cancelled replay: %v", err)
	}
	called := false
	if err := r.s.replayPages(ctx, sessionOwner("sess-acme"), func([]spaces.ReplayEvent) { called = true }); grpcstatus.Code(err) != codes.Canceled || called {
		t.Errorf("cancelled pages: %v, decided %v", err, called)
	}
}

func TestReplaySpacesLimits(t *testing.T) {
	r := newReplayRig(t)
	r.record(t)
	events, rows := maxReplayEvents, maxReplayRows
	t.Cleanup(func() { maxReplayEvents, maxReplayRows = events, rows })
	maxReplayEvents = 3
	rep := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", All: true})
	if !rep.GetTruncated() || rep.GetEvents() != 3 {
		t.Errorf("event cap = %v", rep)
	}
	maxReplayEvents, maxReplayRows = events, 2
	rep = replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-acme", All: true})
	if !rep.GetTruncated() || len(rep.GetRows()) != 2 || rep.GetEvents() != 7 {
		t.Errorf("row cap = %v", rep)
	}
}

// TestRecordedVerdictReadsWouldRule keeps the command log's "would" rule
// and the replay's reading of it in step.
func TestRecordedVerdictReadsWouldRule(t *testing.T) {
	for _, v := range []spaces.Verdict{spaces.Allow, spaces.Refuse} {
		got := spaces.RecordedVerdict(decisionLogged, wouldRule(string(v), string(spaces.RuleOtherJobRepo)))
		if !got.OK || got.Verdict != v || got.Rule != spaces.RuleOtherJobRepo {
			t.Errorf("%s: %+v", v, got)
		}
		if got := spaces.RecordedVerdict(decisionNotEnforced, wouldRule(string(v), "")); !got.OK || got.Verdict != v || got.Rule != "" {
			t.Errorf("%s without a rule: %+v", v, got)
		}
	}
	if got := spaces.RecordedVerdict(toollog.DecisionRefused, "x"); !got.OK || got.Verdict != spaces.Refuse {
		t.Errorf("refused: %+v", got)
	}
	if spaces.RecordedVerdict(decisionLogged, "").OK {
		t.Error("an undecided event read as decided")
	}
}

// replayDB opens a second connection to the orch.db spacesServer made.
func replayDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
