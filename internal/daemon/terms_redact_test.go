package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
	"github.com/willesq/orchestrator/internal/toollog"
)

// fixtureTerms are the terms of the two jobs in TestTermsNeverLeak, and
// texts that contain them (a regex term's text differs from its line).
var fixtureTerms = []struct{ space, line, text string }{
	{"acme", "Falcon", "falcon"},
	{"acme", "Project Kestrel", "project kestrel"},
	{"globex", "Zephyr", "zephyr"},
	{"globex", `re:GX-[0-9]{3}`, "GX-417"},
}

// withTermsPolicy applies a policy with two jobs, each with a terms file
// outside any git tree, and returns the terms files' directory.
func withTermsPolicy(t *testing.T, s *server) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]string{}
	for _, f := range fixtureTerms {
		files[f.space] = append(files[f.space], f.line)
	}
	for space, lines := range files {
		if err := os.WriteFile(filepath.Join(dir, space+".txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	doc := fmt.Sprintf(`{"spaces":[{"name":"acme","kind":"isolated","roots":["/w/acme"],"terms_file":%q},`+
		`{"name":"globex","kind":"isolated","roots":["/w/globex"],"terms_file":%q}]}`,
		filepath.Join(dir, "acme.txt"), filepath.Join(dir, "globex.txt"))
	d := draft(t, s, doc)
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeReq is a Write tool call whose content is text.
func writeReq(session, cwd, text string) *orchdv1.RecordToolEventRequest {
	in, _ := json.Marshal(map[string]string{"file_path": cwd + "/notes.md", "content": text})
	return &orchdv1.RecordToolEventRequest{Event: "pre-tool-use", SessionId: session, Cwd: cwd,
		ToolName: "Write", ToolInput: in, ToolInputSize: int64(len(in)), ToolUseId: "tu"}
}

// argvReq is a Codex exec_command call running argv.
func argvReq(session, cwd string, argv []string) *orchdv1.RecordToolEventRequest {
	in, _ := json.Marshal(map[string]any{"cmd": argv})
	return &orchdv1.RecordToolEventRequest{Event: "pre-tool-use", SessionId: session, Cwd: cwd,
		ToolName: "exec_command", ToolInput: in, ToolInputSize: int64(len(in)), ToolUseId: "tu"}
}

// leaks reports the fixture terms found in text, case-insensitively.
func leaks(text string) []string {
	var out []string
	low := strings.ToLower(text)
	for _, f := range fixtureTerms {
		if strings.Contains(low, strings.ToLower(f.text)) || !strings.HasPrefix(f.line, "re:") && strings.Contains(low, strings.ToLower(f.line)) {
			out = append(out, f.text)
		}
	}
	return out
}

// TestTermsNeverLeak is SP-044.2.a1 (INV-07): with the fixture terms in
// commands, paths and directories, no term appears in a refusal, the
// command log, orchd's log or status output.
func TestTermsNeverLeak(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s, dirs := pinsServer(t)
	withToolLog(t, s)
	withTermsPolicy(t, s)
	if _, err := s.PinSession(inClaude, pinReq("sess-a", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	ctx := pinnedCtx(t, s, "sess-a")
	for _, f := range fixtureTerms {
		for _, req := range []*orchdv1.RecordToolEventRequest{
			bashReq("sess-a", dirs[0], `git commit -m "launch `+f.text+` today"`),
			bashReq("sess-a", "/w/acme/"+f.text, "cat /w/acme/"+f.text+"/notes.md && echo "+f.text),
			{Event: "cwd-changed", SessionId: "sess-a", OldCwd: dirs[0], NewCwd: "/w/" + f.text + "/x", Cwd: "/w/" + f.text},
			writeReq("sess-a", dirs[0], "a\n"+f.text+" b\t"+f.text+" & "+strings.Repeat("x", 3000)+" "+f.text),
			// Written paths and orgs are targets too (Writes, GHOrgs).
			bashReq("sess-a", dirs[0], `echo hi > "/w/acme/`+f.text+`.txt"; gh api "orgs/`+f.text+`"`),
			// A Codex argv whose words are joined back into the term.
			argvReq("sess-a", dirs[0], append(append([]string{"grep", "-rn"}, strings.Fields(f.text)...), ".")),
		} {
			if _, err := s.RecordToolEvent(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
	}
	flushToolLog(t, s)
	recs := records(t, s, sessionOwner("sess-a"))
	if len(recs) < 4*len(fixtureTerms) {
		t.Fatalf("%d records stored", len(recs))
	}
	for _, r := range recs {
		b, _ := json.Marshal(r)
		if l := leaks(string(b)); len(l) > 0 {
			t.Errorf("record %d holds terms %q: %s", r.Seq, l, b)
		}
	}
	if !strings.Contains(recs[0].Command, "[term]") {
		t.Errorf("command not redacted as [term]: %q", recs[0].Command)
	}
	st, err := s.Status(context.Background(), &orchdv1.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if l := leaks(prototext.Format(st)); len(l) > 0 {
		t.Errorf("status holds terms %q", l)
	}
	// A refusal of each term names its space, line and hash only.
	res, _ := s.spaces.Current()
	lists, errs := spaces.SpaceTerms(res, "")
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	for _, f := range fixtureTerms {
		for _, l := range lists {
			hit, ok := l.Match("we ship " + f.text + " soon")
			if !ok {
				continue
			}
			reason := spaces.OutboundResult{Refuse: true, Rule: spaces.RuleOutboundTerm, Hit: hit, Kind: "git push"}.Reason()
			if lk := leaks(reason); len(lk) > 0 || !strings.Contains(reason, "line") {
				t.Errorf("refusal %q holds terms %q", reason, lk)
			}
		}
	}
	if l := leaks(logs.String()); len(l) > 0 {
		t.Errorf("orchd's log holds terms %q", l)
	}
}

// TestTermsWithheldWhenUnreadable: while a terms file cannot be read, the
// command log keeps no text that could hold a term.
func TestTermsWithheldWhenUnreadable(t *testing.T) {
	s, dirs := pinsServer(t)
	withToolLog(t, s)
	dir := withTermsPolicy(t, s)
	if _, err := s.PinSession(inClaude, pinReq("sess-a", dirs[0], "")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "globex.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordToolEvent(pinnedCtx(t, s, "sess-a"), bashReq("sess-a", dirs[0], "echo zephyr")); err != nil {
		t.Fatal(err)
	}
	flushToolLog(t, s)
	recs := records(t, s, sessionOwner("sess-a"))
	if len(recs) != 1 || recs[0].Command != withheldTerms || recs[0].Cwd != withheldTerms || recs[0].Targets != withheldTerms {
		t.Errorf("records %+v", recs)
	}
	// No policy: nothing is redacted.
	r := toollog.Record{Command: "echo zephyr"}
	v := (&server{}).termsView()
	v.withhold(&r)
	if r.Command != "echo zephyr" || v.text("zephyr") != "zephyr" || string(v.json([]byte("zephyr"))) != "zephyr" {
		t.Errorf("no spaces: %q", r.Command)
	}
	if termsKey(nil, "") != "" {
		t.Error("no policy has a terms key")
	}
	if k := termsKey(mustResolve(t, `{"spaces":[{"name":"a","kind":"isolated","terms_file":"~/t.txt"}]}`), "/h"); !strings.Contains(k, "/h/t.txt") {
		t.Errorf("terms key %q", k)
	}
}

func mustResolve(t *testing.T, doc string) *spaces.Resolved {
	t.Helper()
	p, err := spaces.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	r, err := spaces.Validate(p, spaces.Env{Home: "/h", InGitTree: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
