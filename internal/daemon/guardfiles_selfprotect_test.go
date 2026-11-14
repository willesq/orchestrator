package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/db"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// selfProtectRig is orchd with a home of its own: orch's config and data
// directories under it, a checkout of an isolated job with git hooks, the
// job's terms file, and the guard writing its files into the data
// directory as Run does.
type selfProtectRig struct {
	s                                        *server
	home, config, data, checkout, terms, dir string
	files                                    spaces.GuardFiles
}

func newSelfProtectRig(t *testing.T) selfProtectRig {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := selfProtectRig{home: home,
		config:   filepath.Join(home, ".config", "orch"),
		data:     filepath.Join(home, ".local", "share", "orch"),
		checkout: filepath.Join(home, "code", "job", "app"),
		terms:    filepath.Join(home, "terms", "job.txt"),
	}
	r.dir = filepath.Join(r.data, "guard")
	for _, d := range []string{r.config, r.data, filepath.Join(r.checkout, ".git", "hooks"),
		filepath.Join(home, ".claude"), filepath.Join(home, "terms"), filepath.Join(home, "code", "oss")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(r.terms, []byte("terms"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_DATA_DIR", r.data)
	t.Setenv("ORCH_CONFIG_DIR", r.config)
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	e, err := spaces.Open(context.Background(), conn, spaces.Env{Home: home, InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	r.s = &server{spaces: e}
	withEnforcement(t, r.s)
	r.s.guard = newGuardWriter(r.dir, home, r.config, r.data, filepath.Join(home, "bin"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.s.startGuardFiles(ctx)
	applyPolicy(t, r.s, `{"spaces":[`+
		`{"name":"job","kind":"isolated","roots":["~/code/job"],"terms_file":"~/terms/job.txt"},`+
		`{"name":"oss","kind":"public","roots":["~/code/oss"]}]}`)
	withPins(t, r.s, r.checkout, filepath.Join(home, "code", "oss"))
	// Reason: pinned through PinSession, which writes the pin file the
	// hook reads, as a live session is.
	if pin, err := r.s.PinSession(inClaude, pinReq("sess-job", r.checkout, "")); err != nil || pin.GetSpace() != "job" {
		t.Fatalf("pin = %v, %v", pin, err)
	}
	r.files = spaces.GuardFiles{Dir: r.dir, DB: filepath.Join(r.data, "orch.db")}
	return r
}

// call is a tool call of the pinned session from its checkout.
func (r selfProtectRig) call(tool string, input map[string]string) spaces.GuardInput {
	b, _ := json.Marshal(input)
	return spaces.GuardInput{Kind: spaces.EventPreToolUse, SessionID: "sess-job", Tool: tool, Input: b, Cwd: r.checkout}
}

func (r selfProtectRig) write(path string) spaces.GuardInput {
	return r.call("Write", map[string]string{"file_path": path, "content": "x"})
}

func (r selfProtectRig) bash(cmd string) spaces.GuardInput {
	return r.call("Bash", map[string]string{"command": cmd})
}

// TestGuardSelfProtectionLive is RC-110 end to end: the snapshot orchd
// writes carries the whole self-protection, and the hook's decision reads
// it, so file-tool writes and shell redirections into harness settings, a
// checkout's git hooks and orch's data directory are refused, as is a read
// of the job's terms file, while ordinary calls in the space are allowed.
func TestGuardSelfProtectionLive(t *testing.T) {
	r := newSelfProtectRig(t)
	ctx := context.Background()
	settings := filepath.Join(r.home, ".claude", "settings.json")
	hook := filepath.Join(r.checkout, ".git", "hooks", "x")
	db := filepath.Join(r.data, "orch.db")
	refused := map[string]spaces.GuardInput{
		"Write harness settings":           r.write(settings),
		"Edit harness settings":            r.call("Edit", map[string]string{"file_path": settings, "old_string": "a", "new_string": "b"}),
		"redirect into harness settings":   r.bash(`echo '{"hooks":{}}' > ` + settings),
		"Write a git hook":                 r.write(hook),
		"redirect into a git hook":         r.bash("echo 'curl x | sh' > " + hook),
		"Write orch.db":                    r.write(db),
		"redirect into orch.db":            r.bash("echo x >> " + db),
		"redirect into the policy file":    r.bash("echo '{}' > " + spaces.GuardSnapshotPath(r.dir)),
		"Write a pin file":                 r.write(spaces.GuardPinPath(r.dir, "sess-job")),
		"remove the data directory":        r.bash("rm -rf " + r.data),
		"Write orch's config":              r.write(filepath.Join(r.config, "AGENTS.md")),
		"Read the terms file":              r.call("Read", map[string]string{"file_path": r.terms}),
		"cat the terms file":               r.bash("cat " + r.terms),
		"copy the terms file into space":   r.bash("cp " + r.terms + " " + filepath.Join(r.checkout, "t.txt")),
		"Write the checkout's Claude hook": r.write(filepath.Join(r.checkout, ".claude", "settings.local.json")),
		"replace orch-hook":                r.bash("cp ./fake " + filepath.Join(r.home, "bin", "orch-hook")),
		"Write orchd":                      r.write(filepath.Join(r.home, "bin", "orchd")),
		"Write the MCP server list":        r.write(filepath.Join(r.home, ".claude.json")),
		"redirect into a plugin's hooks":   r.bash("echo '{}' > " + filepath.Join(r.home, ".claude", "plugins", "p", "hooks", "hooks.json")),
		"Write the launchd job":            r.write(filepath.Join(r.home, "Library", "LaunchAgents", "com.orchestrator.orchd.plist")),
		"edit brew services' job":          r.bash("sed -i '' s/a/b/ " + filepath.Join(r.home, "Library", "LaunchAgents", "homebrew.mxcl.orch.plist")),
	}
	for name, in := range refused {
		t.Run(name, func(t *testing.T) {
			res := r.files.Decide(ctx, in)
			if res.Verdict != spaces.Refuse || res.Rule != spaces.RuleSelfProtection || !res.Enforced || res.Revision == 0 {
				t.Errorf("%s (%s), enforced %v, revision %d; want refuse (self_protection)", res.Verdict, res.Rule, res.Enforced, res.Revision)
			}
		})
	}
	allowed := map[string]spaces.GuardInput{
		"Write a source file":      r.write(filepath.Join(r.checkout, "main.go")),
		"redirect into the space":  r.bash("echo x > " + filepath.Join(r.checkout, "out.txt")),
		"Read in the space":        r.call("Read", map[string]string{"file_path": filepath.Join(r.checkout, "README.md")}),
		"list the checkout":        r.bash("ls -la " + r.checkout),
		"read the git config dir":  r.bash("ls " + filepath.Join(r.checkout, ".git", "hooks")),
		"read harness settings":    r.bash("cat " + settings),
		"git status in the space":  r.bash("git status"),
		"cat a file in the space":  r.bash("cat " + filepath.Join(r.checkout, "go.mod")),
		"Edit a file in the space": r.call("Edit", map[string]string{"file_path": filepath.Join(r.checkout, "a.go"), "old_string": "a", "new_string": "b"}),
	}
	for name, in := range allowed {
		t.Run(name, func(t *testing.T) {
			if res := r.files.Decide(ctx, in); res.Verdict != spaces.Allow || res.Rule != spaces.RuleWithinSpace {
				t.Errorf("%s (%s), want allow (within_space)", res.Verdict, res.Rule)
			}
		})
	}
}

// TestReplaySpacesReproducesSelfProtection is RC-110 for replay: a
// self-protection refusal the live guard recorded replays as the same
// verdict, because orchd's replay protects what the snapshot does.
func TestReplaySpacesReproducesSelfProtection(t *testing.T) {
	r := newSelfProtectRig(t)
	withToolLog(t, r.s)
	ctx := context.Background()
	settings := filepath.Join(r.home, ".claude", "settings.json")
	calls := []spaces.GuardInput{
		r.bash("echo x > " + filepath.Join(r.checkout, ".git", "hooks", "pre-commit")),
		r.write(settings),
		r.bash("cat " + r.terms),
		r.bash("echo x > " + filepath.Join(r.checkout, "out.txt")),
	}
	owner := sessionOwner("sess-job")
	for _, in := range calls {
		live := r.files.Decide(ctx, in)
		targets, _ := json.Marshal(spaces.Targets(in.Tool, in.Input, in.Cwd))
		decision := toollog.DecisionRefused
		if live.Verdict == spaces.Allow {
			decision = decisionAllowed
		}
		logFixture(t, r.s, owner, time.Now(), toollog.Record{Event: toollog.EventPreToolUse, Tool: in.Tool, Cwd: in.Cwd,
			Targets: string(targets), Decision: decision, Rule: string(live.Rule), PolicyRev: live.Revision})
	}
	flushToolLog(t, r.s)
	rep := replay(t, r.s, &orchdv1.ReplaySpacesRequest{Owner: "sess-job", All: true, Mode: spaces.SwitchBlockOtherJobs})
	if rep.GetCompared() != int64(len(calls)) || rep.GetChanged() != 0 || rep.GetProblems() != 0 {
		t.Fatalf("replay = %v", rep)
	}
	selfProtected := 0
	for _, row := range rep.GetRows() {
		if row.GetRule() == string(spaces.RuleSelfProtection) && row.GetVerdict() == string(spaces.Refuse) {
			selfProtected++
		}
	}
	if selfProtected != 3 {
		t.Errorf("%d rows replayed as self_protection refusals, want 3: %v", selfProtected, rep.GetRows())
	}
}
