package daemon

import (
	"context"
	"log"
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/queue"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// orch records other than memory (SP-026, INV-01) are kept within the
// caller's readable spaces with the same scope memory reads use
// (readscope.go): the PR cache and GitHub items, PR detail, review watches
// and landings by their repo's owner, and the task ledger by the space a
// task recorded at dispatch (D-46). The operator, and every caller while no
// policy is applied, sees every record. See docs/internals/daemon.md
// "Records by space".

// recordScope is the scope ctx's caller reads records under for method:
// readScope's, except that the operator shown to be Orch.app reads every
// record without readScope's log line while memory filtering is switched
// off, since the app polls these RPCs.
func (s *server) recordScope(ctx context.Context, method string) readScope {
	if c := callerOf(ctx); c.kind == callerOperator && c.operatorBy != "" {
		return readScope{}
	}
	return s.readScope(ctx, method)
}

// seesSpace reports whether the scope reads a record placed in space sp.
// A filtered scope reads only its own spaces, so a record with no space
// (made before a policy, or by a caller orchd does not track) is the
// operator's alone, as quarantine is (INV-03).
func (r readScope) seesSpace(sp string) bool {
	if !r.filtered() {
		return true
	}
	return sp != "" && slices.Contains(r.spaces, sp)
}

// orgSpaceOf is the space GitHub account owner's records belong to under
// policy res: the space its org is bound to, or Quarantine for an owner no
// space binds (INV-03), and "" with no policy applied. Fork parents play
// no part: the owner is the repo's own.
func orgSpaceOf(res *spaces.Resolved, owner string) string {
	if !res.Configured() {
		return ""
	}
	if sp, ok := res.GitHubOrgSpace(owner); ok {
		return sp
	}
	return spaces.Quarantine
}

// seesOwner reports whether the scope reads the records of GitHub account
// owner.
func (r readScope) seesOwner(owner string) bool {
	if !r.filtered() {
		return true
	}
	return r.seesSpace(orgSpaceOf(r.res, owner))
}

// seesRepo reports whether the scope reads the records of repo
// ("owner/name").
func (r readScope) seesRepo(repo string) bool {
	owner, _, _ := strings.Cut(repo, "/")
	return r.seesOwner(owner)
}

// seesGitHubURL reports whether the scope reads the records of a GitHub
// URL (https://github.com/<owner>/...). A URL that names no owner is read
// only by an unfiltered scope.
func (r readScope) seesGitHubURL(url string) bool {
	if !r.filtered() {
		return true
	}
	rest, ok := strings.CutPrefix(url, "https://github.com/")
	owner, _, _ := strings.Cut(rest, "/")
	return ok && owner != "" && r.seesOwner(owner)
}

// taskSpaces reads the space each task recorded at dispatch, from its
// durable workflow input (D-46), once per task for one call. A task whose
// input cannot be read has none.
type taskSpaces struct {
	ctl  *queue.Control
	seen map[string]string
}

func (s *server) taskSpaces() *taskSpaces {
	return &taskSpaces{ctl: s.ctl, seen: map[string]string{}}
}

// knownTaskSpaces caches each task's recorded space for orchd's life: it
// is fixed at dispatch, and reading it costs a read of the task's workflow
// history, which a WatchTasks tick would repeat for every task. Only a
// space found is kept, so a task whose input could not be read yet is read
// again next time.
var knownTaskSpaces sync.Map

// of is task id's recorded space, "" for none.
func (ts *taskSpaces) of(ctx context.Context, id string) string {
	if sp, ok := ts.seen[id]; ok {
		return sp
	}
	if sp, ok := knownTaskSpaces.Load(id); ok {
		return sp.(string)
	}
	sp := ""
	if ts.ctl != nil && ts.ctl.Runs != nil {
		if in, err := ts.ctl.Runs.RecordedInput(ctx, id); err == nil {
			sp = in.Space
		}
	}
	ts.seen[id] = sp
	if sp != "" {
		knownTaskSpaces.Store(id, sp)
	}
	return sp
}

// tasksFor keeps of list the tasks scope reads.
func (s *server) tasksFor(ctx context.Context, scope readScope, list []queue.Task) []queue.Task {
	if !scope.filtered() {
		return list
	}
	ts := s.taskSpaces()
	out := list[:0:0]
	for _, t := range list {
		if scope.seesSpace(ts.of(ctx, t.ID)) {
			out = append(out, t)
		}
	}
	return out
}

// seesTask reports whether ctx's caller reads task id under scope.
func (s *server) seesTask(ctx context.Context, scope readScope, id string) bool {
	return !scope.filtered() || scope.seesSpace(s.taskSpaces().of(ctx, id))
}

// errTaskNotFound is what a caller gets for a task of a space it does not
// act in: the answer for a task that does not exist, so the two cannot be
// told apart.
func errTaskNotFound(id string) error {
	return grpcstatus.Errorf(codes.NotFound, "%v: %s", queue.ErrNotFound, id)
}

// changesTask reports whether ctx's caller may retry, cancel or rate task
// id under scope: the operator, any caller with no policy applied, or a
// caller acting in the space the task recorded (RC-708). Reading a task's
// space is not enough to rerun it: a session that reads a public space
// does not rerun that space's tasks.
func (s *server) changesTask(ctx context.Context, scope readScope, id string) bool {
	if !scope.filtered() {
		return true
	}
	sp := callerSpace(callerOf(ctx))
	// Reason: quarantine holds what no space could place, the operator's
	// alone (INV-03), though unplaced sessions are pinned to it.
	return sp != "" && sp != spaces.Quarantine && sp == s.taskSpaces().of(ctx, id)
}

// changesRepo reports whether ctx's caller may start, change or stop work
// on repo's PRs under scope, such as a review watch whose task reads the
// owner's space (SP-026): the operator, any caller with no policy applied,
// or a caller acting in the space the owner is bound to.
func changesRepo(ctx context.Context, scope readScope, repo string) bool {
	if !scope.filtered() {
		return true
	}
	owner, _, _ := strings.Cut(repo, "/")
	sp := callerSpace(callerOf(ctx))
	return sp != "" && sp != spaces.Quarantine && sp == orgSpaceOf(scope.res, owner)
}

// errNoPR is what a caller gets for a PR of a space it may not see or act
// in: it reads as a PR GitHub does not show it.
var errNoPR = grpcstatus.Error(codes.NotFound, "GitHub: no such pull request for this caller")

// spaceDeleter forgets a source in one space's collection only
// (implemented by *rag.Service).
type spaceDeleter interface {
	DeleteSourceIn(ctx context.Context, uri, space string) (int, error)
}

// deleteInOwnSpace is DeleteSource for a caller other than Orch.app while
// a spaces policy is applied (D-77): a session deletes the source only from
// its own space's collection, and is told only how many chunks went from
// there, so it can neither delete nor learn of another space's copy.
// Anyone with no space of its own, or pinned to quarantine (which holds
// what no space could place), deletes nothing.
func (s *server) deleteInOwnSpace(ctx context.Context, c resolvedCaller, uri string) (*orchdv1.DeleteSourceResponse, error) {
	own := ""
	if c.kind == callerSession {
		own = c.pin.Space
	}
	if own == "" || own == spaces.Quarantine {
		log.Printf("orchd: refused DeleteSource from %s: it has no space of its own to delete from", readerLabel(c))
		return nil, grpcstatus.Error(codes.PermissionDenied,
			"while a spaces policy is applied a session deletes only from its own space, and Orch.app from any (D-77)")
	}
	d, ok := s.rag.(spaceDeleter)
	if !ok {
		return nil, grpcstatus.Error(codes.Unavailable, "the memory store cannot delete by space")
	}
	log.Printf("orchd: DELETE source %q from %s's own space — permanent, no undo", uri, readerLabel(c))
	n, err := d.DeleteSourceIn(ctx, uri, own)
	if err != nil {
		// Reason: the store's error names collections, and so spaces.
		log.Printf("orchd: DELETE source %q failed: %v", uri, err)
		return nil, grpcstatus.Error(codes.Unavailable, "the memory store could not be changed (see orchd log)")
	}
	log.Printf("orchd: DELETE source %q removed %d chunk(s)", uri, n)
	return &orchdv1.DeleteSourceResponse{Deleted: count32(n)}, nil
}
