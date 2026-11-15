package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/session"
	"github.com/willesq/orchestrator/internal/spaces"
)

// seedBase stores one chunk of uri in the base collection, as a writer before
// SP-023 or SP-024 left it: no space, no labels_v, origin only when given.
func (q *memQdrant) seedBase(uri, writer string, origin map[string]any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.collections["c"] == nil {
		q.collections["c"] = map[string]map[string]any{}
		q.vectors["c"] = map[string]any{}
	}
	p := map[string]any{"source_uri": uri, "source_type": "note", "chunk_index": float64(0), "chunk_total": float64(1),
		"text": "stored before spaces: " + uri, "ingest_hash": "h"}
	if writer != "" {
		p["writer"] = writer
	}
	if origin != nil {
		p["origin"] = origin
	}
	id := uri + "#0"
	q.collections["c"][id] = p
	q.vectors["c"][id] = map[string]any{"dense": []any{1.0, 0.0}}
}

// relabelEnv is the plan's example policy over a memQdrant, with a server
// that can relabel it.
func relabelEnv(t *testing.T) (writesEnv, *server) {
	t.Helper()
	e := newWritesEnv(t, orgNodes{})
	e.s.migrator = e.svc
	return e, e.s
}

// TestRelabelPlacesStoredContent: SP-027.1.a1 and INV-03 through orchd's
// placer. Content stored before placement is placed from its stored origin
// or, without one, from its writer and URI and the session table, and moved
// to its space's collection; content that cannot be placed (no origin, a
// client note with no writer space, a URI its writer never makes, a session
// never pinned, a conflict) goes to quarantine, never to public or any other
// space.
func TestRelabelPlacesStoredContent(t *testing.T) {
	e, s := relabelEnv(t)
	ctx := context.Background()
	co := filepath.Join(e.code, "company1", "app", "README.md")
	writeFile(t, co, "# app")
	link := filepath.Join(e.code, "company1", "app", "link.md")
	if err := os.Symlink(filepath.Join(e.code, "willesq", "notes.md"), link); err != nil {
		t.Fatal(err)
	}
	pin := sessionPin{SessionID: "s-acme", Harness: "claude", Process: procstat.Identity{PID: 7, Started: time.Unix(1, 0)},
		FirstCwd: filepath.Join(e.code, "company1", "app"), Space: "acme", Revision: e.rev, PinnedAt: time.Now().UTC()}
	if err := s.pins.insert(ctx, pin); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s-acme", "s-unpinned"} {
		if err := session.WriteRecord(paths.SessionsDir(), &session.Record{SessionID: id, Summary: "x", TranscriptPath: sessionTranscript(t, id)}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		uri, writer string
		origin      map[string]any
		space       string
	}{
		{"repo://acme/app/README.md", rag.WriterDocs, nil, "acme"},
		{"repo://oss-org/lib/README.md", rag.WriterDocs, nil, "oss"},
		{"repo://oss-org/secret/README.md", rag.WriterDocs, nil, spaces.Quarantine},
		{"checkout://" + co, rag.WriterSources, nil, "acme"},
		{"https://docs.oss.dev/guide", rag.WriterSources, nil, "oss"},
		{"file://" + co, rag.WriterFile, nil, "acme"},
		{"session://s-acme/record", rag.WriterSession, nil, "acme"},
		{"session://s-unpinned/transcript", rag.WriterSession, nil, spaces.Quarantine},
		{"session://s-norecord/record", rag.WriterSession, nil, spaces.Quarantine},
		{"repo://acme/app/note", rag.WriterClient, nil, spaces.Quarantine},
		{"repo://acme/app/old", "", nil, spaces.Quarantine},
		{"manual://not-a-repo", rag.WriterDocs, nil, spaces.Quarantine},
		{"checkout://relative/path", rag.WriterSources, nil, spaces.Quarantine},
		{"stored-origin", rag.WriterDocs, map[string]any{"system": "github", "repo_owner": "globex", "repo_name": "x", "visibility": "private"}, "globex"},
		{"checkout://" + filepath.Join(e.code, "company1", "x", "README.md"), rag.WriterSources, nil, spaces.Quarantine},
		// A hand-off path that is a link now, or is gone, cannot say what was
		// placed: the live write placed what the link resolved to then.
		{"file://" + link, rag.WriterFile, nil, spaces.Quarantine},
		{"file://" + filepath.Join(e.code, "company1", "app", "gone.md"), rag.WriterFile, nil, spaces.Quarantine},
	}
	for _, c := range cases {
		e.q.seedBase(c.uri, c.writer, c.origin)
	}
	dry, err := s.relabel(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Applied || dry.Stale != len(cases) || dry.Changed != len(cases) {
		t.Fatalf("dry run = %+v", dry)
	}
	if got := e.q.storedIn("c", cases[0].uri); len(got) != 1 {
		t.Fatal("a dry run moved a chunk")
	}

	rep, err := s.relabel(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 0 || rep.Changed != len(cases) || rep.Rev != e.rev {
		t.Fatalf("report = %+v", rep)
	}
	for _, c := range cases {
		in := e.q.storedIn(rag.SpaceCollection("c", c.space), c.uri)
		if len(in) != 1 {
			t.Errorf("%s (writer %q): not in %s's collection (sources: %v)", c.uri, c.writer, c.space, e.q.uris())
			continue
		}
		if in[0]["space"] != c.space {
			t.Errorf("%s: space = %v, want %s", c.uri, in[0]["space"], c.space)
		}
		if v, _ := in[0]["labels_v"].(float64); int64(v) != e.rev {
			t.Errorf("%s: labels_v = %v, want %d", c.uri, in[0]["labels_v"], e.rev)
		}
	}
	// A reconstructed origin is stored, so the next relabel needs no guess.
	if o, _ := e.q.storedIn("c__acme", "repo://acme/app/README.md")[0]["origin"].(map[string]any); o["repo_owner"] != "acme" || o["system"] != "github" {
		t.Fatalf("stored origin = %v", o)
	}
	if again, err := s.relabel(ctx, true); err != nil || again.Changed != 0 {
		t.Fatalf("second run = %+v, %v", again, err)
	}

	// A docs repo GitHub cannot be asked about now stays stale, to be
	// tried again, rather than being placed with an unknown visibility.
	e.q.seedBase("repo://acme/unknown/README.md", rag.WriterDocs, nil)
	rep, err = s.relabel(ctx, true)
	if err != nil || rep.Failed != 1 || rep.Changed != 0 {
		t.Fatalf("unknown repo: %+v, %v", rep, err)
	}
	if got := e.q.storedIn("c", "repo://acme/unknown/README.md"); len(got) != 1 || got[0]["labels_v"] != nil {
		t.Fatalf("unknown repo's chunk = %v", got)
	}
}

// TestRelabelRefusals: a relabel needs an applied policy that verifies and
// the RAG service, and never runs beside a migration.
func TestRelabelRefusals(t *testing.T) {
	ctx := context.Background()
	if _, err := (&server{}).relabel(ctx, false); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("no service: %v", err)
	}
	noPolicy := spacesServerWith(t, orgNodes{})
	noPolicy.migrator = rag.NewService(unitEmbedder{}, rag.NewQdrantStore("http://127.0.0.1:1", "c", ""), 200, 0)
	if _, err := noPolicy.relabel(ctx, false); err == nil || !strings.Contains(err.Error(), "no spaces policy") {
		t.Fatalf("no policy: %v", err)
	}
	_, s := relabelEnv(t)
	migrating.Lock()
	_, err := s.relabel(ctx, false)
	migrating.Unlock()
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("beside a migration: %v", err)
	}
}

// TestRelabelPlacerRefusesAChangedPolicy: a revision applied mid-run leaves
// the rest stale for the next run instead of stamping them with the old one.
func TestRelabelPlacerRefusesAChangedPolicy(t *testing.T) {
	e, s := relabelEnv(t)
	p := relabelPlacer{w: &writePlacer{spaces: s.spaces, pins: s.pins, repos: s.repos}, rev: e.rev + 1}
	m := rag.Meta{SourceURI: "repo://acme/app/README.md", Writer: rag.WriterDocs}
	if err := p.place(context.Background(), &m); err == nil || m.Space != "" {
		t.Fatalf("placed under a stale revision: %q, %v", m.Space, err)
	}
	p.rev = e.rev
	if err := p.place(context.Background(), &m); err != nil || m.Space != "acme" {
		t.Fatalf("placed = %q, %v", m.Space, err)
	}
	// Applying a changed policy is a new revision: everything is stale.
	pol := mustPolicy(t, s)
	pol.Spaces = append(pol.Spaces, spaces.Space{Name: "extra", Kind: spaces.Private})
	doc, _ := json.Marshal(pol)
	d := draft(t, s, string(doc))
	if _, err := s.ApplySpaces(operatorCtx, &orchdv1.ApplySpacesRequest{ReviewedRevision: d.GetRevision()}); err != nil {
		t.Fatal(err)
	}
	if _, rev, _ := s.spaces.CurrentRevision(); rev == e.rev {
		t.Fatalf("applying left revision %d", rev)
	}
	if err := p.place(context.Background(), &m); err == nil {
		t.Fatal("placed under a revision that is no longer applied")
	}
}

// mustPolicy returns the applied policy.
func mustPolicy(t *testing.T, s *server) spaces.Policy {
	t.Helper()
	res, _, err := s.spaces.CurrentRevision()
	if err != nil || res == nil {
		t.Fatalf("current revision: %v", err)
	}
	return res.Policy
}

// TestStoredOriginClaudeConfig: a hand-off of Claude Code's own config is
// placed as the file under ~/.claude, and a URI that climbs out of it gets
// no origin.
func TestStoredOriginClaudeConfig(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	o := storedOrigin(context.Background(), nil, rag.Meta{SourceURI: "claude://CLAUDE.md", Writer: rag.WriterFile})
	if o.System != rag.OriginFS || o.Path != filepath.Join(home, ".claude", "CLAUDE.md") {
		t.Fatalf("origin = %+v", o)
	}
	if o := storedOrigin(context.Background(), nil, rag.Meta{SourceURI: "claude://../.ssh/id", Writer: rag.WriterFile}); o.System != "" {
		t.Fatalf("an escaping URI got origin %+v", o)
	}
	if o := storedOrigin(context.Background(), nil, rag.Meta{SourceURI: "file://rel/x.md", Writer: rag.WriterFile}); o.System != "" {
		t.Fatalf("a relative file URI got origin %+v", o)
	}
	if o := storedOrigin(context.Background(), nil, rag.Meta{SourceURI: "session://x", Writer: rag.WriterSession}); o.System != "" {
		t.Fatalf("a session URI with no kind got origin %+v", o)
	}
}

// TestRelabelRPC: the Relabel RPC answers the operator with counts and space
// names, refuses sessions and queued tasks (the report names spaces), and
// reports a refused run as FailedPrecondition.
func TestRelabelRPC(t *testing.T) {
	e, s := relabelEnv(t)
	e.q.seedBase("repo://acme/app/README.md", rag.WriterDocs, nil)
	for name, ctx := range map[string]context.Context{
		"session": sessionCtx(context.Background(), sessionPin{SessionID: "s-p", Space: "acme", Revision: e.rev}),
		"task":    taskCtx("t-1"),
	} {
		if _, err := s.Relabel(ctx, &orchdv1.RelabelRequest{}); grpcstatus.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s caller: %v", name, err)
		}
	}
	dry, err := s.Relabel(operatorCtx, &orchdv1.RelabelRequest{})
	if err != nil || dry.GetApplied() || dry.GetChanged() != 1 || dry.GetRevision() != e.rev || dry.GetError() != "" {
		t.Fatalf("dry run = %v, %v", dry, err)
	}
	if tr := dry.GetTransitions(); len(tr) != 1 || tr[0].GetFrom() != rag.Unplaced || tr[0].GetTo() != "acme" || tr[0].GetChunks() != 1 {
		t.Fatalf("transitions = %v", tr)
	}
	done, err := s.Relabel(operatorCtx, &orchdv1.RelabelRequest{Apply: true})
	if err != nil || !done.GetApplied() || done.GetMoved() != 1 || len(e.q.storedIn("c__acme", "repo://acme/app/README.md")) != 1 {
		t.Fatalf("apply = %v, %v", done, err)
	}
	// A store that fails mid-run still reports, with a generic error.
	e.q.seedBase("repo://acme/app/OTHER.md", rag.WriterDocs, nil)
	failing := &server{spaces: s.spaces, pins: s.pins, repos: s.repos,
		migrator: rag.NewService(unitEmbedder{}, rag.NewQdrantStore("http://127.0.0.1:1", "c", ""), 200, 0)}
	resp, err := failing.Relabel(operatorCtx, &orchdv1.RelabelRequest{Apply: true})
	if err != nil || resp.GetError() == "" || strings.Contains(resp.GetError(), "127.0.0.1") {
		t.Fatalf("failing store = %v, %v", resp, err)
	}
	if _, err := (&server{}).Relabel(operatorCtx, &orchdv1.RelabelRequest{}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unavailable: %v", err)
	}
}
