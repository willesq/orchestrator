package spaces

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fileFixture is a home with two jobs and their guard files.
type fileFixture struct {
	home, dir, db string
	files         GuardFiles
}

// guardPolicy binds ~/code public, ~/code/work to job "work" and
// ~/code/other to job "other".
func guardPolicy() Policy {
	return Policy{Spaces: []Space{
		{Name: "oss", Kind: Public, Roots: []string{"~/code"}},
		{Name: "work", Kind: Isolated, Roots: []string{"~/code/work"}, GitHubOrgs: []string{"workorg"}},
		{Name: "other", Kind: Isolated, Roots: []string{"~/code/other"}, GitHubOrgs: []string{"otherorg"}},
	}}
}

func newFileFixture(t *testing.T, mode string) fileFixture {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{"code/work/app", "code/other/app"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := fileFixture{home: home, dir: filepath.Join(t.TempDir(), "guard"), db: filepath.Join(t.TempDir(), "orch.db")}
	f.files = GuardFiles{Dir: f.dir, DB: f.db}
	p := guardPolicy()
	f.writeSnapshot(t, GuardSnapshot{Revision: 3, Policy: &p, Home: home, Guard: GuardSwitch{State: mode}})
	if err := WriteGuardPin(f.dir, GuardPinFile{SessionID: "s1", Space: "work", Revision: 3, Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fileFixture) writeSnapshot(t *testing.T, s GuardSnapshot) {
	t.Helper()
	if err := WriteGuardSnapshot(f.dir, s); err != nil {
		t.Fatal(err)
	}
}

func (f fileFixture) bash(cmd string) GuardInput {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return GuardInput{Kind: EventPreToolUse, SessionID: "s1", Tool: "Bash", Input: in, Cwd: filepath.Join(f.home, "code/work/app")}
}

func TestGuardFilesRoundTrip(t *testing.T) {
	f := newFileFixture(t, SwitchBlockOtherJobs)
	s, err := ReadGuardSnapshot(f.dir)
	if err != nil || s.Revision != 3 || s.Policy == nil || len(s.Policy.Spaces) != 3 || s.Home != f.home || s.Version != GuardSnapshotVersion {
		t.Fatalf("snapshot read back as %+v, %v", s, err)
	}
	p, err := ReadGuardPin(f.dir, "s1")
	if err != nil || p.Space != "work" || p.Revision != 3 || p.Harness != "claude" {
		t.Fatalf("pin read back as %+v, %v", p, err)
	}
	for _, path := range []string{f.dir, filepath.Join(f.dir, "pins"), GuardSnapshotPath(f.dir), GuardPinPath(f.dir, "s1")} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if st.IsDir() {
			want = 0o700
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", path, st.Mode().Perm(), want)
		}
	}
}

func TestGuardFilesRefuseBadInput(t *testing.T) {
	f := newFileFixture(t, SwitchBlockOtherJobs)
	for _, id := range []string{"", "../x", "a/b", strings.Repeat("a", 129)} {
		if GuardPinPath(f.dir, id) != "" {
			t.Errorf("session ID %q names a pin file", id)
		}
		if err := WriteGuardPin(f.dir, GuardPinFile{SessionID: id}); err == nil {
			t.Errorf("wrote a pin file for %q", id)
		}
		if _, err := ReadGuardPin(f.dir, id); err == nil {
			t.Errorf("read a pin file for %q", id)
		}
	}
	// A pin file must name its own session.
	if err := os.WriteFile(GuardPinPath(f.dir, "s2"), []byte(`{"session_id":"s1","space":"work"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGuardPin(f.dir, "s2"); err == nil {
		t.Error("a pin file naming another session was read")
	}
	if err := os.WriteFile(GuardPinPath(f.dir, "s3"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGuardPin(f.dir, "s3"); err == nil {
		t.Error("a damaged pin file was read")
	}
	// The snapshot must be a regular file of bounded size and known version.
	snap := GuardSnapshotPath(f.dir)
	for name, write := range map[string]func() error{
		"damaged":   func() error { return os.WriteFile(snap, []byte("{"), 0o600) },
		"version":   func() error { return os.WriteFile(snap, []byte(`{"version":99}`), 0o600) },
		"oversized": func() error { return os.WriteFile(snap, make([]byte, maxGuardFile+1), 0o600) },
		"directory": func() error { _ = os.Remove(snap); return os.Mkdir(snap, 0o700) },
		"symlink": func() error {
			real := filepath.Join(t.TempDir(), "real.json")
			if err := os.WriteFile(real, []byte(`{"version":1}`), 0o600); err != nil {
				return err
			}
			return os.Symlink(real, snap)
		},
	} {
		_ = os.RemoveAll(snap)
		if err := write(); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadGuardSnapshot(f.dir); err == nil {
			t.Errorf("%s snapshot was read", name)
		}
	}
	_ = os.RemoveAll(snap)
	if _, err := ReadGuardSnapshot(f.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing snapshot: %v, want ErrNotExist", err)
	}
	// A directory that cannot be created fails the write.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteGuardSnapshot(filepath.Join(blocked, "guard"), GuardSnapshot{}); err == nil {
		t.Error("wrote a snapshot under a file")
	}
}

func TestGuardFilesDecide(t *testing.T) {
	ctx := context.Background()
	f := newFileFixture(t, SwitchBlockOtherJobs)
	cases := []struct {
		name string
		in   GuardInput
		want Verdict
		rule GuardRule
	}{
		{"within the session's space", f.bash("ls " + filepath.Join(f.home, "code/work")), Allow, RuleWithinSpace},
		{"another job's folder", f.bash("cat " + filepath.Join(f.home, "code/other/app/x")), Refuse, RuleOtherJobPath},
		{"another job's repo", f.bash("gh api repos/otherorg/app"), Refuse, RuleOtherJobRepo},
		{"the guard's own files", GuardInput{Kind: EventPreToolUse, SessionID: "s1", Tool: "Write", Cwd: filepath.Join(f.home, "code/work"),
			Input: mustJSON(t, map[string]string{"file_path": GuardSnapshotPath(f.dir), "content": "{}"})}, Allow, RuleWithinSpace},
		{"no pin", func() GuardInput { in := f.bash("ls"); in.SessionID = "unpinned"; return in }(), Refuse, RulePinMissing},
		{"cwd-changed into another job", GuardInput{Kind: EventCwdChanged, SessionID: "s1", Cwd: filepath.Join(f.home, "code/work"),
			NewCwd: filepath.Join(f.home, "code/other")}, Allow, RuleOtherJobPath},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := f.files.Decide(ctx, c.in)
			if r.Verdict != c.want || r.Rule != c.rule || r.Revision != 3 {
				t.Errorf("got %s (%s) rev %d, want %s (%s)", r.Verdict, r.Rule, r.Revision, c.want, c.rule)
			}
		})
	}
	// The snapshot's protected paths reach Decide.
	p := guardPolicy()
	f.writeSnapshot(t, GuardSnapshot{Revision: 3, Policy: &p, Home: f.home, Protected: []string{f.dir}})
	r := f.files.Decide(ctx, cases[3].in)
	if r.Verdict != Refuse || r.Rule != RuleSelfProtection {
		t.Errorf("write to a protected path: %s (%s)", r.Verdict, r.Rule)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGuardFilesNoPolicy: without spaces nothing is guarded, with or
// without a snapshot or pin (INV-10).
func TestGuardFilesNoPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFileFixture(t, SwitchStrict)
	f.writeSnapshot(t, GuardSnapshot{Home: f.home, Guard: GuardSwitch{State: SwitchStrict}})
	in := f.bash("cat /etc/hosts")
	in.SessionID = "unpinned"
	if r := f.files.Decide(ctx, in); r.Verdict != Allow || r.Rule != RuleNoPolicy {
		t.Errorf("no spaces: %s (%s)", r.Verdict, r.Rule)
	}
	_ = os.Remove(GuardSnapshotPath(f.dir))
	if r := f.files.Decide(ctx, in); r.Verdict != Allow || r.Rule != RuleNoPolicy {
		t.Errorf("no snapshot and no orch.db: %s (%s)", r.Verdict, r.Rule)
	}
}

// TestGuardFilesUnreadablePolicy: a snapshot that cannot be read, or that
// marks a broken or invalid policy, refuses in enforcing modes and allows
// when the guard is off or logging (INV-13).
func TestGuardFilesUnreadablePolicy(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{SwitchOff, SwitchLog, SwitchBlockOtherJobs, SwitchStrict} {
		want := Allow
		if GuardMode(mode).Enforcing() {
			want = Refuse
		}
		t.Run(mode, func(t *testing.T) {
			f := newFileFixture(t, mode)
			check := func(what string) {
				t.Helper()
				r := f.files.Decide(ctx, f.bash("ls"))
				if r.Verdict != want || r.Would != Refuse || r.Rule != RulePolicyUnreadable {
					t.Errorf("%s: %s would %s (%s), want %s", what, r.Verdict, r.Would, r.Rule, want)
				}
			}
			f.writeSnapshot(t, GuardSnapshot{Revision: 3, Broken: true, Home: f.home, Guard: GuardSwitch{State: mode}})
			check("broken policy")
			bad := Policy{Spaces: []Space{{Name: "Bad Name", Kind: Isolated}}}
			f.writeSnapshot(t, GuardSnapshot{Revision: 3, Policy: &bad, Home: f.home, Guard: GuardSwitch{State: mode}})
			check("invalid policy")
			p := guardPolicy()
			f.writeSnapshot(t, GuardSnapshot{Revision: 3, Policy: &p, Home: f.home, Protected: []string{"relative"}, Guard: GuardSwitch{State: mode}})
			check("relative protected path")
		})
	}
}

// TestGuardFilesSnapshotMissing: with no snapshot, an orch.db that applied
// a policy refuses unless its guard switch is off or log; one that never
// applied allows.
func TestGuardFilesSnapshotMissing(t *testing.T) {
	ctx := context.Background()
	conn := openStore(t)
	dbPath := filepath.Join(os.Getenv("ORCH_DATA_DIR"), "orch.db")
	files := GuardFiles{Dir: filepath.Join(t.TempDir(), "guard"), DB: dbPath}
	in := GuardInput{Kind: EventPreToolUse, SessionID: "s1", Tool: "Bash", Input: []byte(`{"command":"ls"}`), Cwd: "/"}
	if r := files.Decide(ctx, in); r.Verdict != Allow || r.Rule != RuleNoPolicy {
		t.Errorf("never applied: %s (%s)", r.Verdict, r.Rule)
	}
	e := openEngine(t, conn)
	saveAndApply(t, e, validPolicy)
	if ok, err := HasApplied(ctx, dbPath); !ok || err != nil {
		t.Fatalf("HasApplied = %v, %v", ok, err)
	}
	if r := files.Decide(ctx, in); r.Verdict != Refuse || r.Rule != RulePolicyUnreadable || r.Mode != GuardBlockOtherJobs {
		t.Errorf("applied, no snapshot: %s (%s) in %s", r.Verdict, r.Rule, r.Mode)
	}
	if _, _, err := setSwitch(ctx, conn, LayerGuard, SwitchLog, "op", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r := files.Decide(ctx, in); r.Verdict != Allow || r.Would != Refuse || r.Mode != GuardLog {
		t.Errorf("applied, no snapshot, guard log: %s would %s in %s", r.Verdict, r.Would, r.Mode)
	}
	// A store that cannot be read refuses as strict.
	garbage := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	files.DB = garbage
	if r := files.Decide(ctx, in); r.Verdict != Refuse || r.Mode != GuardStrict {
		t.Errorf("unreadable store: %s in %s", r.Verdict, r.Mode)
	}
	if _, err := HasApplied(ctx, garbage); err == nil {
		t.Error("HasApplied read a damaged store")
	}
	empty := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := HasApplied(ctx, empty); ok || err != nil {
		t.Errorf("store without tables: %v, %v", ok, err)
	}
	// A damaged snapshot is not a missing one: with nothing applied it
	// still refuses while enforcing.
	files.DB = empty
	if err := os.MkdirAll(files.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GuardSnapshotPath(files.Dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := files.Decide(ctx, in); r.Verdict != Refuse || r.Rule != RulePolicyUnreadable {
		t.Errorf("damaged snapshot: %s (%s)", r.Verdict, r.Rule)
	}
}

// TestGuardSnapshotTimedSwitch: a timed switch in the snapshot reverts at
// its end even when orchd does not rewrite the snapshot.
func TestGuardSnapshotTimedSwitch(t *testing.T) {
	end := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := GuardSnapshot{Guard: GuardSwitch{State: SwitchOff, Until: end, Previous: SwitchStrict}}
	if m := s.Mode(end.Add(-time.Second)); m != GuardOff {
		t.Errorf("before the end: %s", m)
	}
	if m := s.Mode(end); m != GuardStrict {
		t.Errorf("at the end: %s", m)
	}
	if m := (GuardSnapshot{}).Mode(end); m != GuardBlockOtherJobs {
		t.Errorf("no switch: %s", m)
	}
	f := newFileFixture(t, SwitchOff)
	p := guardPolicy()
	f.writeSnapshot(t, GuardSnapshot{Revision: 3, Policy: &p, Home: f.home, Guard: s.Guard})
	f.files.Now = func() time.Time { return end }
	var seen GuardMode
	f.files.OnMode = func(m GuardMode) { seen = m }
	r := f.files.Decide(context.Background(), f.bash("cat "+filepath.Join(f.home, "code/other/x")))
	if r.Verdict != Refuse || seen != GuardStrict {
		t.Errorf("after the end: %s, mode seen %s", r.Verdict, seen)
	}
}

func TestTimedOut(t *testing.T) {
	for mode, want := range map[GuardMode]Verdict{"": Refuse, GuardOff: Allow, GuardLog: Allow,
		GuardBlockOtherJobs: Refuse, GuardStrict: Refuse, "bogus": Refuse} {
		r := TimedOut(mode)
		if r.Verdict != want || r.Would != Refuse || r.Rule != RuleGuardTimeout {
			t.Errorf("TimedOut(%q) = %s would %s (%s), want %s", mode, r.Verdict, r.Would, r.Rule, want)
		}
	}
}
