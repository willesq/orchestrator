package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Every memory write is placed by orchd (SP-023, INV-02, INV-03): the
// writer reports its content's origin, writePlacer runs the classifier under
// the applied policy, and the chunk is stored with its origin, the space it
// was placed in and that policy's revision (labels_v). A writer never names
// a space, and no RPC field reaches one. See docs/internals/memory.md
// "Placement of writes".

// errWriteConflict refuses a write whose origin two spaces of equal
// strictness claim: nothing is indexed (SP-022).
var errWriteConflict = errors.New("two spaces claim it equally, so it is not indexed until the operator resolves the conflict")

// writePlacer places writes. Its fields are read only; it is shared by every
// writer.
type writePlacer struct {
	// spaces is the enforced policy; nil places everything in Shared.
	spaces *spaces.Engine
	// pins finds a session's pin, so its records go where it is pinned.
	pins *pinRegistry
	// repos answers a checkout's repo owner and visibility.
	repos *repoLookups
}

// place is a rag.PlaceFunc.
func (w *writePlacer) place(ctx context.Context, m *rag.Meta) error {
	_, err := w.placeMeta(ctx, m)
	return err
}

// placeMeta places m, setting its space and policy revision, and returns
// the classifier's placement when a configured policy decided it.
func (w *writePlacer) placeMeta(ctx context.Context, m *rag.Meta) (spaces.Placement, error) {
	if w.spaces == nil {
		m.Space = spaces.Shared
		return spaces.Placement{}, nil
	}
	res, rev, err := w.spaces.CurrentRevision()
	switch {
	case errors.Is(err, spaces.ErrBroken):
		// Reason: with no policy that verifies, nothing can be placed, and
		// unplaceable content is quarantined, never shared (INV-03).
		m.Space = spaces.Quarantine
		return spaces.Placement{}, nil
	case err != nil:
		return spaces.Placement{}, err
	case !res.Configured():
		m.Space, m.LabelsV = spaces.Shared, rev
		return spaces.Placement{}, nil
	}
	if m.Writer == rag.WriterFile {
		// Reason: an agent picks which files are handed off, so placing
		// them spends none of GitHub's lookup budget.
		ctx = withCachedRepoLookups(ctx)
	}
	p, err := w.placement(ctx, res, m.Origin)
	if err != nil {
		return spaces.Placement{}, fmt.Errorf("place %s: %w", m.SourceURI, err)
	}
	if remoteSteered(m, p) {
		// Reason: an agent can write a .git naming any org beside a file it
		// edits, or register such a folder as a source, so a checkout's
		// remote alone never places a local file. Such a file waits in
		// quarantine for the operator (until SP-029 gives hand-offs the
		// editing session's space).
		p = quarantined("a local file placed only by its checkout's remote")
	}
	if !mayWritePublic(m.Writer) && sessionContentUnderPublicRoot(res, p) {
		// Reason: the public space is read by every session, so only the
		// writers that verify content public reach it. Content a session
		// wrote under a public root (an edited file) goes where sessions
		// under a public root write (D-22, SP-030).
		p = publicRootSessions(res)
	}
	if p.Outcome == spaces.Conflict {
		return p, fmt.Errorf("%s: %w", m.SourceURI, errWriteConflict)
	}
	m.Space, m.LabelsV = p.Space, rev
	return p, nil
}

// remoteSteered reports whether p placed a local file by its checkout's
// remote where a .git the writer could have made decides: under no root at
// all, or over the root it is under. Reason (SP-030, D-65): nothing on disk
// places content, so a session that writes a .git naming another job's org
// beside files it registers as a source cannot move them into that job's
// space; they wait in quarantine for the operator.
func remoteSteered(m *rag.Meta, p spaces.Placement) bool {
	if m.Origin.System != rag.OriginFS {
		return false
	}
	return p.Rule == spaces.PlaceRemote || p.Rule == spaces.PlaceRemoteStricter
}

// placement classifies o under res.
func (w *writePlacer) placement(ctx context.Context, res *spaces.Resolved, o rag.Origin) (spaces.Placement, error) {
	var so spaces.Origin
	switch o.System {
	case rag.OriginGitHub:
		so = spaces.Origin{System: spaces.SystemGitHub,
			Remote: &spaces.Repo{Owner: o.RepoOwner, Name: o.RepoName, Visibility: spaces.Visibility(o.Visibility)}}
	case rag.OriginFS:
		if o.Path == "" {
			return quarantined("a file write names no path"), nil
		}
		so = pathOrigin(ctx, w.repos, o.Path)
	case rag.OriginWeb:
		so = spaces.Origin{System: spaces.SystemWeb, URL: o.URL}
	case rag.OriginSession:
		return w.sessionPlacement(ctx, res, o)
	case rag.OriginClient:
		so = spaces.Origin{System: spaces.SystemClient, WriterSpace: o.WriterSpace}
	default:
		return quarantined(fmt.Sprintf("unknown origin system %q", o.System)), nil
	}
	return w.spaces.PlaceUnder(ctx, res, so)
}

// sessionPlacement places a session's own record or transcript where the
// session is pinned (INV-05). Only a pin orchd made decides: a session it
// never pinned, or one whose transcript is not its own (sessionOrigin), is
// quarantined, since its ID and directory come from files any process of the
// user can write.
func (w *writePlacer) sessionPlacement(ctx context.Context, res *spaces.Resolved, o rag.Origin) (spaces.Placement, error) {
	if w.pins == nil || o.Session == "" {
		return quarantined("the write is tied to no pinned session"), nil
	}
	pin, ok, err := w.pins.get(ctx, o.Session)
	switch {
	case err != nil:
		return spaces.Placement{}, err
	case !ok:
		return quarantined("the session has no pin"), nil
	}
	// Reason: a pin made under another policy can name a space this one
	// lacks, or none; sessions never write to public.
	if sp, known := res.Space(pin.Space); known && sp.Kind != spaces.Public {
		return spaces.Placement{Space: pin.Space, Outcome: spaces.Placed, Rule: spaces.PlaceWriter}, nil
	}
	return quarantined("the session's pin names no space of the applied policy"), nil
}

// mayWritePublic reports whether writer may place content in a public
// space: only the docs sync and the sources refresh, which place by what
// GitHub or a bound web prefix verifies and by the policy's public_paths
// (SP-076). Session records, summaries, hand-offs and client notes never do (SP-030.1.a4).
func mayWritePublic(writer string) bool {
	return writer == rag.WriterDocs || writer == rag.WriterSources
}

// sessionContentUnderPublicRoot reports whether p places content in a
// public space, or would but for the public-root proof (no_public_proof)
// with no remote GitHub says is private or unknown. Content of a repo not
// verified public stays in quarantine.
func sessionContentUnderPublicRoot(res *spaces.Resolved, p spaces.Placement) bool {
	if p.Outcome == spaces.Placed {
		s, _ := res.Space(p.Space)
		return s.Kind == spaces.Public
	}
	return p.Rule == spaces.PlaceNoPublicProof && (p.Remote == nil || p.Remote.Repo.Visibility == spaces.VisibilityPublic)
}

// publicRootSessions is where session content that would otherwise be
// public goes: the policy's public_root_sessions space, or quarantine.
func publicRootSessions(res *spaces.Resolved) spaces.Placement {
	p := spaces.Placement{Space: res.PublicRootSessions, Outcome: spaces.Placed, Rule: spaces.PlacePublicRootSession,
		Reason: "session content never goes to a public space; it goes to public_root_sessions"}
	if p.Space == "" || p.Space == spaces.Quarantine {
		p.Space, p.Outcome = spaces.Quarantine, spaces.Quarantined
	}
	return p
}

// leavesPublic reports whether a source stored in space stored must leave
// the public space (SP-030): stored is a public space of the applied policy
// and m, written now, would be placed outside every public space. A write
// that cannot be placed now is not proven public, so it leaves too. With no
// policy, or one that does not verify, nothing is moved.
func (w *writePlacer) leavesPublic(ctx context.Context, stored string, m rag.Meta) bool {
	if w == nil || w.spaces == nil || stored == "" {
		return false
	}
	res, _, err := w.spaces.CurrentRevision()
	if err != nil || !res.Configured() {
		return false
	}
	if s, ok := res.Space(stored); !ok || s.Kind != spaces.Public {
		return false
	}
	p, err := w.placeMeta(ctx, &m)
	if err != nil {
		return true
	}
	if s, ok := res.Space(m.Space); ok && s.Kind == spaces.Public {
		return false
	}
	return !awaitsGitHub(p)
}

// awaitsGitHub reports whether p keeps a folder public_paths list out of
// public only because GitHub gave no answer about its repo (an outage, or a
// spent lookup budget). Reason (RC-402): such a blip must not delete and
// re-embed a listed folder's public copy; it leaves once GitHub answers
// that the repo is private or refuses to show it.
func awaitsGitHub(p spaces.Placement) bool {
	return p.Rule == spaces.PlaceNotVerifiedPublic && p.PublicPath != "" &&
		p.Remote != nil && p.Remote.Repo.Unchecked && p.Remote.Repo.Visibility == spaces.VisibilityUnknown
}

func quarantined(reason string) spaces.Placement {
	return spaces.Placement{Space: spaces.Quarantine, Outcome: spaces.Quarantined, Rule: spaces.PlaceUnmatched, Reason: reason}
}

// writerSpace is the space a client write's caller writes to: its session's
// pin, or a queued task's space recorded at dispatch (D-46). The operator and
// unidentified callers have none here, so their notes are quarantined until
// the operator retags them (SP-023.1.a3).
func writerSpace(c resolvedCaller) string {
	switch {
	case c.kind == callerSession:
		return c.pin.Space
	case c.kind == callerTask && c.task.TaskID != "":
		return c.task.Space
	}
	return ""
}

// clientOrigin is what orchd reports about a client's write: who the caller
// is and the space its session writes to. A page's URL is kept for a later
// relabel, but a client write is placed by its writer alone.
func clientOrigin(ctx context.Context, uri string) rag.Origin {
	c := callerOf(ctx)
	o := rag.Origin{System: rag.OriginClient, Caller: string(c.kind), WriterSpace: writerSpace(c)}
	if strings.HasPrefix(uri, "https://") || strings.HasPrefix(uri, "http://") {
		o.URL = uri
	}
	return o
}

// errReplaceRefused refuses a client write over a source placed in another
// space. It says nothing of that space.
var errReplaceRefused = grpcstatus.Error(codes.FailedPrecondition,
	"this source_uri cannot be written by this caller; store the note under another source_uri")

// mayReplace reports whether a client write with origin o may replace a
// source stored in space stored. Reason: a write replaces and prunes every
// chunk under its URI, so without this a caller could overwrite or erase
// another space's source by naming it (SP-023).
func (s *server) mayReplace(ctx context.Context, o rag.Origin, stored string) error {
	m := rag.Meta{Origin: o}
	if err := (&writePlacer{spaces: s.spaces, pins: s.pins, repos: s.repos}).place(ctx, &m); err != nil {
		log.Printf("orchd: Ingest: placing a replacement: %v", err)
		return errReplaceRefused
	}
	if m.Space != stored {
		return errReplaceRefused
	}
	return nil
}
