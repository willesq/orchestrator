package daemon

import (
	"context"
	"errors"
	"log"
	"path"
	"slices"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Every memory read path applies the caller's readable spaces inside orchd
// (SP-025, INV-01, INV-02): Search, ListSources, ListCollections,
// BrowseChunks, GetDocument and ListDocs, and so every client built on them
// (rag_search, rag_list_sources, auto-RAG, session recall, orch rag why,
// orch rag browse). The spaces come from the resolved caller
// alone (caller_session.go); a request's filters (tags, source_uri, scope,
// cwd) only narrow what those spaces hold. See docs/internals/daemon.md
// "Reading memory by space".

// readScope is what one caller's memory reads may see.
type readScope struct {
	// spaces are the spaces read. nil reads every collection, as reads did
	// before spaces: no policy is applied, the caller is the operator, or
	// memory filtering is switched off. An empty, non-nil list reads
	// nothing.
	spaces []string
	// res is the applied policy when one with spaces is enforced, and
	// labelsV its revision: a filtered read sees only chunks labelled under
	// it (INV-08), so content placed under an earlier policy is unreadable
	// until 'orch rag relabel' (SP-027) moves it.
	res     *spaces.Resolved
	labelsV int64
}

// filtered reports whether the scope narrows what is read.
func (r readScope) filtered() bool { return r.spaces != nil }

// readScope decides what ctx's caller reads for method. While a policy is
// applied and memory filtering is switched off (SP-070), every read is
// logged as not enforced.
func (s *server) readScope(ctx context.Context, method string) readScope {
	if s.spaces == nil {
		return readScope{}
	}
	res, rev, err := s.spaces.CurrentRevision()
	switch {
	case errors.Is(err, spaces.ErrBroken):
		// Reason: the newest apply does not verify, so no space's readers
		// are known; nothing is read until the operator applies again.
		return readScope{spaces: []string{}}
	case err != nil:
		log.Printf("orchd: %s: the spaces policy could not be read, so nothing is read: %v", rpcName(method), err)
		return readScope{spaces: []string{}}
	case !res.Configured():
		// INV-10: with no spaces, reads are as they were before spaces.
		return readScope{}
	}
	c := callerOf(ctx)
	if s.enforcement.state(spaces.LayerMemory) == spaces.SwitchOff {
		log.Printf("orchd: %s for %s: not enforced (memory filtering is switched off)", rpcName(method), readerLabel(c))
		return readScope{}
	}
	sc := readScope{spaces: readableBy(res, c), res: res}
	if !sc.filtered() {
		return sc
	}
	if rev <= 0 {
		// Reason: revision 0 would turn the labels_v filter off (INV-08);
		// an applied policy always has a revision, so this reads nothing.
		log.Printf("orchd: %s: the applied spaces policy has no revision, so nothing is read", rpcName(method))
		return readScope{spaces: []string{}}
	}
	sc.labelsV = rev
	return sc
}

// readableBy lists the spaces caller c reads under res.
//
//   - The operator shown to be Orch.app by its code signature (D-71)
//     reads every space.
//   - A session reads what its pin's space reads (D-16): itself, every
//     public space, and the private spaces it lists. A pin is only ever
//     taken by a harness in its own session's tree (SP-041 refuses starting
//     one elsewhere; an orphaned harness gets no pin), so a session never
//     reads beyond what its original session may (D-38).
//   - A queued task reads what the space recorded at dispatch reads (D-46).
//   - Anyone else, including an operator with no evidence of the user and a
//     task orchd does not track, reads public spaces only.
func readableBy(res *spaces.Resolved, c resolvedCaller) []string {
	switch {
	case c.kind == callerOperator && c.operatorBy != "":
		return nil
	case c.kind == callerSession:
		return readableFrom(res, c.pin.Space)
	case c.kind == callerTask && c.task.TaskID != "":
		return readableFrom(res, c.task.Space)
	}
	return readableFrom(res, "")
}

// readableFrom lists every public space and each space reader may read
// under res, sorted (spaces.Resolved.ReadableFrom).
func readableFrom(res *spaces.Resolved, reader string) []string { return res.ReadableFrom(reader) }

// readerLabel names c for orchd's log, never with a request's own words.
func readerLabel(c resolvedCaller) string {
	switch c.kind {
	case callerSession:
		return "session " + printableID(c.pin.SessionID)
	case callerTask:
		if c.task.TaskID == "" {
			return "an untracked queued task"
		}
		return "task " + printableID(c.task.TaskID)
	case callerOperator:
		if c.operatorBy == "" {
			return "the operator (not Orch.app)"
		}
		return "the operator"
	}
	return "an unidentified caller"
}

// rpcName is a method's short name, for log lines.
func rpcName(method string) string { return path.Base(method) }

// dispatchSpace is the space whose memory a task dispatched by ctx's caller
// reads (D-46): the dispatching session's pin, or for the operator the
// placement of the task's working directory (Quarantine for a scratch
// directory, which no root holds). Anyone else records none, so the task
// reads public memory only. With no policy applied nothing is recorded.
func (s *server) dispatchSpace(ctx context.Context, cwd string) string {
	if !s.spacesConfigured() {
		return ""
	}
	c := callerOf(ctx)
	switch {
	case c.kind == callerSession:
		return c.pin.Space
	case c.kind != callerOperator || c.operatorBy == "":
		return ""
	case cwd == "":
		return spaces.Quarantine
	}
	sp, _, err := s.placeSession(ctx, cwd)
	if err != nil {
		log.Printf("orchd: Dispatch: placing the task's directory failed, so it reads public memory only: %v", err)
		return ""
	}
	return sp
}

// storeError is what a read path answers when the memory store fails. The
// store's own error names collections, whose names hold space names
// (RC-350), so for a filtered reader it stays in orchd's log; anyone else
// gets it as before (INV-10).
func (r readScope) storeError(rpc string, err error) error {
	if !r.filtered() {
		return err
	}
	log.Printf("orchd: %s: reading the memory store: %v", rpc, err)
	return grpcstatus.Error(codes.Unavailable, "the memory store could not be read (see orchd log)")
}

// narrowDocs keeps in a ListDocs reply only what scope may see: the files
// already listed (read from its spaces), the owners and repos those files
// are in, and owners whose GitHub org is bound to a space it reads with
// their repos. An owner listed only for some of its repos loses its own
// error, and the sync's progress for it keeps only a repo that is listed and
// no counts; for an owner not listed, the progress is empty.
func narrowDocs(out *orchdv1.ListDocsResponse, scope readScope) {
	repos := map[string]bool{}
	for _, f := range out.Files {
		repos[f.Owner+"/"+f.Repo] = true
	}
	ownerReadable := func(owner string) bool {
		sp, ok := scope.res.GitHubOrgSpace(owner)
		return ok && slices.Contains(scope.spaces, sp)
	}
	kept := map[string]bool{}
	owners := out.Owners[:0]
	for _, o := range out.Owners {
		whole := ownerReadable(o.Owner)
		rs := o.Repos[:0]
		for _, r := range o.Repos {
			if whole || repos[o.Owner+"/"+r.Repo] {
				rs = append(rs, r)
			}
		}
		o.Repos = rs
		if !whole {
			// Reason: an owner's own error can name repos the caller
			// cannot read.
			o.Error = ""
		}
		if whole || len(rs) > 0 {
			owners = append(owners, o)
			kept[o.Owner] = true
		}
	}
	out.Owners = owners
	switch sy := out.Sync; {
	case sy == nil || sy.Owner == "" || ownerReadable(sy.Owner):
	case !kept[sy.Owner]:
		out.Sync = &orchdv1.DocsSyncState{}
	default:
		// Reason: an owner listed for some of its repos may be syncing one
		// the caller cannot read, and its totals count those too.
		sy.Done, sy.Total = 0, 0
		if !repos[sy.Owner+"/"+sy.Repo] {
			sy.Repo = ""
		}
	}
}
