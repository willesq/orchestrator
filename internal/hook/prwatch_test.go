package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// bashResult builds a PostToolUse payload for a Bash call.
func bashResult(t *testing.T, command, stdout, stderr string) Payload {
	t.Helper()
	resp, _ := json.Marshal(map[string]any{"stdout": stdout, "stderr": stderr, "interrupted": false, "isImage": false})
	return Payload{ToolName: "Bash", ToolInput: toolInput(t, map[string]string{"command": command}), ToolResponse: resp}
}

// prWatchHome points HOME and the data dir at temp dirs and returns the
// registry path, created with body unless body is empty.
func prWatchHome(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	path := filepath.Join(home, prWatchFile)
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

type registry struct {
	Other string `json:"other"`
	PRs   []struct {
		URL     string `json:"url"`
		AddedAt string `json:"added_at"`
		Note    string `json:"note"`
	} `json:"prs"`
}

func readRegistry(t *testing.T, path string) registry {
	t.Helper()
	var r registry
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("registry is not JSON: %v\n%s", err, b)
	}
	return r
}

const u1 = "https://github.com/willesq/orchestrator/pull/901"

func TestPRWatchRegistersCreatedPR(t *testing.T) {
	path := prWatchHome(t, `{"other":"keep","prs":[{"url":"https://github.com/o/r/pull/1","added_at":"2026-01-01T00:00:00Z","note":"mine"}]}`)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	p := bashResult(t, "rtk gh pr create --draft --fill", "Creating draft pull request\n\n"+u1+"\n", "")
	if err := postToolUse(p); err != nil {
		t.Fatal(err)
	}
	r := readRegistry(t, path)
	if len(r.PRs) != 2 || r.PRs[0].Note != "mine" || r.Other != "keep" || r.PRs[1].URL != u1 {
		t.Fatalf("registry %+v", r)
	}
	if _, err := time.Parse(time.RFC3339, r.PRs[1].AddedAt); err != nil || !strings.HasSuffix(r.PRs[1].AddedAt, "Z") {
		t.Errorf("added_at %q is not ISO 8601 UTC", r.PRs[1].AddedAt)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640 kept", info.Mode().Perm())
	}
	before, _ := os.ReadFile(path)
	_ = postToolUse(p)
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a duplicate URL changed the file")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != prWatchFile {
			t.Errorf("left behind %s", e.Name())
		}
	}
}

func TestPRWatchSkips(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		payload func(t *testing.T) Payload
	}{
		{"missing registry is not created", "", func(t *testing.T) Payload { return bashResult(t, "gh pr create", u1, "") }},
		{"unparseable registry is left alone", `{"prs": [ broken`, func(t *testing.T) Payload { return bashResult(t, "gh pr create", u1, "") }},
		{"registry without a prs array is left alone", `{"prs": {}}`, func(t *testing.T) Payload { return bashResult(t, "gh pr create", u1, "") }},
		{"null prs is left alone", `{"prs": null}`, func(t *testing.T) Payload { return bashResult(t, "gh pr create", u1, "") }},
		{"no URL in the output", `{"prs": []}`, func(t *testing.T) Payload { return bashResult(t, "gh pr create", "HTTP 422", "boom") }},
		{"other gh pr commands", `{"prs": []}`, func(t *testing.T) Payload { return bashResult(t, "gh pr edit 5 --title x", u1, "") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := prWatchHome(t, tt.body)
			if err := postToolUse(tt.payload(t)); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if tt.body == "" {
				if !os.IsNotExist(err) {
					t.Errorf("registry created: %s", got)
				}
				return
			}
			if string(got) != tt.body {
				t.Errorf("registry changed to %s", got)
			}
		})
	}
}

func TestPRURLFromResponse(t *testing.T) {
	stderr, _ := json.Marshal(map[string]string{"stdout": "", "stderr": "a pull request for branch \"x\" into branch \"main\" already exists:\nhttps://github.com/acme/infra/pull/5885"})
	if got := prURLFromResponse(stderr); got != "https://github.com/acme/infra/pull/5885" {
		t.Errorf("stderr: %q", got)
	}
	str, _ := json.Marshal("see https://github.com/a/b/pull/1 then https://github.com/a/b/pull/2")
	if got := prURLFromResponse(str); got != "https://github.com/a/b/pull/2" {
		t.Errorf("string response: %q", got)
	}
	if got := prURLFromResponse(nil); got != "" {
		t.Errorf("empty: %q", got)
	}
	if got := prURLFromResponse(json.RawMessage(`{"stdout":"https://github.com/a/b/issues/3"}`)); got != "" {
		t.Errorf("issue URL matched: %q", got)
	}
}

func TestRegisterPRConcurrent(t *testing.T) {
	path := prWatchHome(t, `{"prs": []}`)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := registerPR(path, fmt.Sprintf("https://github.com/o/r/pull/%d", 950+i), time.Now()); err != nil {
				t.Errorf("register: %v", err)
			}
		}()
	}
	wg.Wait()
	r := readRegistry(t, path)
	seen := map[string]bool{}
	for _, e := range r.PRs {
		seen[e.URL] = true
	}
	if len(r.PRs) != 8 || len(seen) != 8 {
		t.Errorf("kept %d entries (%d unique), want 8", len(r.PRs), len(seen))
	}
}

func TestRegisterPRLocking(t *testing.T) {
	path := prWatchHome(t, `{"prs": []}`)
	prevWait := prWatchLockWait
	prWatchLockWait = 50 * time.Millisecond
	t.Cleanup(func() { prWatchLockWait = prevWait })

	lock := path + ".lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := registerPR(path, u1, time.Now()); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("held lock: %v", err)
	}
	// A lock left by a crashed writer is cleared.
	old := time.Now().Add(-2 * prWatchLockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if err := registerPR(path, u1, time.Now()); err != nil {
		t.Fatalf("stale lock: %v", err)
	}
	if r := readRegistry(t, path); len(r.PRs) != 1 {
		t.Errorf("registry %+v", r)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("lock left behind")
	}
}

func TestRegisterPRFollowsSymlink(t *testing.T) {
	path := prWatchHome(t, "")
	target := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(target, []byte(`{"prs": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := registerPR(path, u1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(path); info.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink replaced by a file")
	}
	if _, err := os.Stat(target + ".lock"); !os.IsNotExist(err) {
		t.Error("lock taken next to the target")
	}
	if r := readRegistry(t, target); len(r.PRs) != 1 {
		t.Errorf("target %+v", r)
	}
}

func TestRegisterPRRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pr-watch.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { done <- registerPR(fifo, u1, time.Now()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("FIFO accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("registerPR blocked on a FIFO")
	}
	if err := registerPR(filepath.Join(dir, "missing.json"), u1, time.Now()); err == nil {
		t.Error("missing file accepted")
	}
}

func TestWriteAtomicFailure(t *testing.T) {
	if err := writeAtomic(filepath.Join(t.TempDir(), "missing", "f.json"), []byte("{}"), 0o600); err == nil {
		t.Error("write into a missing directory succeeded")
	}
}
