package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// taskHub fans task changes out to WatchTasks streams. The task ledger and the
// worker's runner publish a task's ID whenever its row or live run changes.
type taskHub struct {
	mu   sync.Mutex
	subs map[*taskSub]struct{}
}

// taskSub is one stream's queue of changed task IDs. When it fills, the
// stream is told to resend a snapshot rather than silently miss changes.
type taskSub struct {
	ch       chan string
	overflow atomic.Bool
}

// subBuffer is how many changes a slow stream may fall behind before it is
// resynced with a snapshot.
const subBuffer = 256

// maxWatchers caps open WatchTasks streams; each costs a buffer and a tick's
// work, and the app needs one.
const maxWatchers = 16

func newTaskHub() *taskHub { return &taskHub{subs: map[*taskSub]struct{}{}} }

// publish tells every stream that id changed. It never blocks: a stream too
// far behind is marked to resync instead.
func (h *taskHub) publish(id string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- id:
		default:
			sub.overflow.Store(true)
		}
	}
}

// subscribe adds a stream, or returns ok false when maxWatchers are open.
func (h *taskHub) subscribe() (sub *taskSub, unsubscribe func(), ok bool) {
	sub = &taskSub{ch: make(chan string, subBuffer)}
	h.mu.Lock()
	if len(h.subs) >= maxWatchers {
		h.mu.Unlock()
		return nil, nil, false
	}
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub, func() {
		h.mu.Lock()
		delete(h.subs, sub)
		h.mu.Unlock()
	}, true
}

// Watch timing: changes arriving together (a status write and a PID report,
// say) go out as one message, and a tick keeps liveness and the stream fresh.
var (
	watchBatch = 100 * time.Millisecond
	watchTick  = 15 * time.Second
)

// WatchTasks streams a snapshot of the newest tasks, then each task whose
// ledger row or live run changes, and a periodic tick of the pending and
// in-progress tasks.
func (s *server) WatchTasks(req *orchdv1.WatchTasksRequest, stream orchdv1.Orchd_WatchTasksServer) error {
	if s.tasks == nil || s.hub == nil {
		return grpcstatus.Error(codes.Unavailable, "task ledger unavailable")
	}
	ctx := stream.Context()
	// Subscribe before the snapshot, so a change made while it is read is
	// sent after it rather than lost.
	sub, unsubscribe, ok := s.hub.subscribe()
	if !ok {
		return grpcstatus.Errorf(codes.ResourceExhausted, "%d task watchers are open already", maxWatchers)
	}
	defer unsubscribe()
	filter := queue.ListFilter{Status: req.GetStatus(), Limit: int(req.GetLimit())}
	if err := s.sendTaskSnapshot(ctx, stream, filter); err != nil {
		return err
	}
	tick := time.NewTicker(watchTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.stopping:
			return grpcstatus.Error(codes.Unavailable, "orchd is shutting down; reconnect")
		case <-tick.C:
			if err := s.sendTaskTick(ctx, stream, filter.Status); err != nil {
				return err
			}
		case id := <-sub.ch:
			ids := s.collectChanges(ctx, sub, id)
			if sub.overflow.Swap(false) {
				if err := s.sendTaskSnapshot(ctx, stream, filter); err != nil {
					return err
				}
				continue
			}
			if err := s.sendTaskChanges(ctx, stream, filter.Status, ids); err != nil {
				return err
			}
		}
	}
}

// collectChanges gathers the IDs that arrive within watchBatch of first,
// each once, in order.
func (s *server) collectChanges(ctx context.Context, sub *taskSub, first string) []string {
	seen := map[string]bool{first: true}
	ids := []string{first}
	timer := time.NewTimer(watchBatch)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ids
		case <-timer.C:
			return ids
		case id := <-sub.ch:
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
}

func (s *server) sendTaskSnapshot(ctx context.Context, stream orchdv1.Orchd_WatchTasksServer, f queue.ListFilter) error {
	list, err := s.tasks.List(ctx, f)
	if err != nil {
		return err
	}
	return stream.Send(&orchdv1.TaskEvent{Kind: "snapshot", Tasks: s.tasksPB(ctx, list)})
}

// sendTaskChanges sends the changed tasks the stream's filter keeps; a task
// that left the filter is sent anyway, so the client can drop or update it.
func (s *server) sendTaskChanges(ctx context.Context, stream orchdv1.Orchd_WatchTasksServer, status string, ids []string) error {
	var list []queue.Task
	for _, id := range ids {
		t, found, err := s.tasks.Get(ctx, id)
		if err != nil {
			return err
		}
		if found {
			list = append(list, t)
		}
	}
	if len(list) == 0 {
		return nil
	}
	return stream.Send(&orchdv1.TaskEvent{Kind: "changed", Tasks: s.tasksPB(ctx, list)})
}

func (s *server) sendTaskTick(ctx context.Context, stream orchdv1.Orchd_WatchTasksServer, status string) error {
	var list []queue.Task
	for _, st := range []string{queue.StatusPending, queue.StatusInProgress} {
		if status != "" && status != st {
			continue
		}
		got, err := s.tasks.List(ctx, queue.ListFilter{Status: st, Limit: 500})
		if err != nil {
			return err
		}
		list = append(list, got...)
	}
	// Reason: a tick only refreshes liveness, so it carries the ID, status
	// and run of each task and none of its text; with many tasks the full
	// rows could pass the client's message size limit every 15 s.
	tasks := make([]*orchdv1.Task, 0, len(list))
	for _, t := range list {
		tasks = append(tasks, &orchdv1.Task{Id: t.ID, Status: t.Status, UpdatedAt: t.UpdatedAt, Run: s.taskRun(ctx, t)})
	}
	return stream.Send(&orchdv1.TaskEvent{Kind: "tick", Tasks: tasks})
}

// tasksPB renders tasks for the wire, each unsettled one with its liveness.
func (s *server) tasksPB(ctx context.Context, list []queue.Task) []*orchdv1.Task {
	out := make([]*orchdv1.Task, 0, len(list))
	for _, t := range list {
		pb := taskPB(t)
		pb.Run = s.taskRun(ctx, t)
		out = append(out, pb)
	}
	return out
}
