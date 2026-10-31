package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/workflows"
)

// fakeWatchStream records what WatchTasks sends.
type fakeWatchStream struct {
	grpc.ServerStream
	ctx    context.Context
	mu     sync.Mutex
	events []*orchdv1.TaskEvent
}

func (f *fakeWatchStream) Context() context.Context { return f.ctx }

func (f *fakeWatchStream) Send(e *orchdv1.TaskEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeWatchStream) find(kind, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.GetKind() != kind {
			continue
		}
		if id == "" {
			return true
		}
		for _, t := range e.GetTasks() {
			if t.GetId() == id {
				return true
			}
		}
	}
	return false
}

func (f *fakeWatchStream) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.GetKind() == kind {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatchTasksStreamsSnapshotChangesAndTicks(t *testing.T) {
	s, store, _ := newTaskServer(t)
	s.hub = newTaskHub()
	store.OnChange = s.hub.publish
	savedBatch, savedTick := watchBatch, watchTick
	watchBatch, watchTick = 5*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { watchBatch, watchTick = savedBatch, savedTick })
	ctx := context.Background()
	if err := store.Create(ctx, workflows.AgentTaskInput{ID: "task-old", Agent: "dev", Body: "x"}); err != nil {
		t.Fatal(err)
	}

	wctx, cancel := context.WithCancel(ctx)
	stream := &fakeWatchStream{ctx: wctx}
	done := make(chan error, 1)
	go func() { done <- s.WatchTasks(&orchdv1.WatchTasksRequest{}, stream) }()

	eventually(t, "the snapshot", func() bool { return stream.find("snapshot", "task-old") })
	// A ledger write is pushed as a change.
	if err := store.Create(ctx, workflows.AgentTaskInput{ID: "task-new", Agent: "dev", Body: "y"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the change", func() bool { return stream.find("changed", "task-new") })
	// The tick carries unsettled tasks with their liveness.
	eventually(t, "a tick", func() bool { return stream.find("tick", "task-new") })
	// A rating publishes too (no rater here: nothing else breaks).
	s.hub.publish("task-old")
	eventually(t, "the rated task", func() bool { return stream.find("changed", "task-old") })
	// A change to a task that no longer exists sends nothing.
	before := stream.count("changed")
	s.hub.publish("task-gone")
	time.Sleep(30 * time.Millisecond)
	if stream.count("changed") != before {
		t.Error("a missing task produced a change event")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WatchTasks ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WatchTasks did not end when the client went away")
	}
	s.hub.mu.Lock()
	n := len(s.hub.subs)
	s.hub.mu.Unlock()
	if n != 0 {
		t.Errorf("%d subscriptions left after the stream ended", n)
	}
}

func TestWatchTasksResyncsAfterOverflow(t *testing.T) {
	s, store, _ := newTaskServer(t)
	s.hub = newTaskHub()
	ctx := context.Background()
	if err := store.Create(ctx, workflows.AgentTaskInput{ID: "task-a", Agent: "dev", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	sub, unsubscribe, ok := s.hub.subscribe()
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer unsubscribe()
	for i := 0; i < subBuffer+5; i++ {
		s.hub.publish("task-a")
	}
	if !sub.overflow.Load() {
		t.Fatal("a full subscriber was not marked to resync")
	}
	// The stream path: an overflowed subscriber gets a snapshot.
	stream := &fakeWatchStream{ctx: ctx}
	ids := s.collectChanges(ctx, sub, <-sub.ch)
	if len(ids) != 1 || ids[0] != "task-a" {
		t.Errorf("collected %v; want task-a once", ids)
	}
	if sub.overflow.Swap(false) {
		if err := s.sendTaskSnapshot(ctx, stream, queue.ListFilter{}); err != nil {
			t.Fatal(err)
		}
	}
	if !stream.find("snapshot", "task-a") {
		t.Error("no resync snapshot")
	}
	var nilHub *taskHub
	nilHub.publish("x") // a nil hub is a no-op
}

func TestWatchTasksFiltersAndUnavailable(t *testing.T) {
	s, store, _ := newTaskServer(t)
	ctx := context.Background()
	if err := s.WatchTasks(&orchdv1.WatchTasksRequest{}, &fakeWatchStream{ctx: ctx}); grpcstatus.Code(err) != codes.Unavailable {
		t.Errorf("without a hub = %v, want Unavailable", err)
	}
	s.hub = newTaskHub()
	if err := store.Create(ctx, workflows.AgentTaskInput{ID: "task-p", Agent: "dev", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	stream := &fakeWatchStream{ctx: ctx}
	if err := s.sendTaskTick(ctx, stream, "complete"); err != nil {
		t.Fatal(err)
	}
	if stream.find("tick", "task-p") {
		t.Error("a tick filtered to complete included a pending task")
	}
	if err := s.sendTaskChanges(ctx, stream, "", nil); err != nil || stream.count("changed") != 0 {
		t.Errorf("no ids should send nothing: %v", err)
	}
}

func TestWatchTasksEndsOnShutdown(t *testing.T) {
	s, _, _ := newTaskServer(t)
	s.hub = newTaskHub()
	stopping := make(chan struct{})
	s.stopping = stopping
	stream := &fakeWatchStream{ctx: context.Background()}
	done := make(chan error, 1)
	go func() { done <- s.WatchTasks(&orchdv1.WatchTasksRequest{}, stream) }()
	eventually(t, "the snapshot", func() bool { return stream.count("snapshot") == 1 })
	close(stopping)
	select {
	case err := <-done:
		if grpcstatus.Code(err) != codes.Unavailable {
			t.Errorf("shutdown ended the stream with %v, want Unavailable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an open stream kept running after shutdown began")
	}
}

func TestWatchTasksCapsWatchers(t *testing.T) {
	s, _, _ := newTaskServer(t)
	s.hub = newTaskHub()
	var unsubs []func()
	for i := 0; i < maxWatchers; i++ {
		_, u, ok := s.hub.subscribe()
		if !ok {
			t.Fatalf("watcher %d refused", i+1)
		}
		unsubs = append(unsubs, u)
	}
	if err := s.WatchTasks(&orchdv1.WatchTasksRequest{}, &fakeWatchStream{ctx: context.Background()}); grpcstatus.Code(err) != codes.ResourceExhausted {
		t.Errorf("watcher past the cap = %v, want ResourceExhausted", err)
	}
	unsubs[0]()
	if _, u, ok := s.hub.subscribe(); !ok {
		t.Error("a freed slot was not reusable")
	} else {
		u()
	}
	for _, u := range unsubs[1:] {
		u()
	}
}

func TestTickCarriesLivenessOnly(t *testing.T) {
	s, store, _ := newTaskServer(t)
	ctx := context.Background()
	if err := store.Create(ctx, workflows.AgentTaskInput{ID: "task-t", Agent: "dev", Body: "a long brief"}); err != nil {
		t.Fatal(err)
	}
	stream := &fakeWatchStream{ctx: ctx}
	if err := s.sendTaskTick(ctx, stream, ""); err != nil {
		t.Fatal(err)
	}
	tk := stream.events[0].GetTasks()[0]
	if tk.GetBody() != "" || tk.GetAgent() != "" || tk.GetStatus() != "pending" || tk.GetRun() == nil {
		t.Errorf("tick task = %+v; want id, status and run only", tk)
	}
}

// blockingServer's GracefulStop waits until Stop is called, like a server
// with an RPC that never returns.
type blockingServer struct{ stopped chan struct{} }

func (b *blockingServer) GracefulStop() { <-b.stopped }
func (b *blockingServer) Stop()         { close(b.stopped) }

func TestStopGracefullyForcesAfterTimeout(t *testing.T) {
	b := &blockingServer{stopped: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		stopGracefully(b, 20*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopGracefully did not force a stop")
	}
	quick := &blockingServer{stopped: make(chan struct{})}
	close(quick.stopped) // GracefulStop returns at once; Stop must not be needed
	stopGracefully(&onceServer{quick}, time.Second)
}

// onceServer fails the test if Stop is called.
type onceServer struct{ *blockingServer }

func (o *onceServer) Stop() { panic("Stop called although GracefulStop returned") }
