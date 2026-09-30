package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

type fakeTasks struct {
	list  []queue.Task
	one   queue.Task
	found bool
	err   error
	// gotFilter records what the handler passed through.
	gotFilter queue.ListFilter
	gotID     string
}

func (f *fakeTasks) List(_ context.Context, filter queue.ListFilter) ([]queue.Task, error) {
	f.gotFilter = filter
	return f.list, f.err
}

func (f *fakeTasks) Get(_ context.Context, id string) (queue.Task, bool, error) {
	f.gotID = id
	return f.one, f.found, f.err
}

func TestServerListSources(t *testing.T) {
	tests := []struct {
		name         string
		rag          fakeRag
		wantSources  int
		wantChunks   int32
		wantDistinct int32
		wantErr      bool
	}{
		{
			name: "summarises the inventory",
			rag: fakeRag{sources: []rag.SourceSummary{
				{SourceURI: "a://1", SourceType: "manual", Chunks: 3, Tags: []string{"t"}},
				{SourceURI: "b://2", SourceType: "web", Chunks: 1},
			}},
			wantSources: 2, wantChunks: 4, wantDistinct: 2,
		},
		{name: "empty store", rag: fakeRag{}, wantSources: 0, wantChunks: 0, wantDistinct: 0},
		{name: "error propagates", rag: fakeRag{err: errors.New("boom")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: tt.rag}
			resp, err := s.ListSources(context.Background(), &orchdv1.ListSourcesRequest{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(resp.GetSources()) != tt.wantSources {
				t.Errorf("sources = %d, want %d", len(resp.GetSources()), tt.wantSources)
			}
			if resp.GetTotalChunks() != tt.wantChunks {
				t.Errorf("total chunks = %d, want %d", resp.GetTotalChunks(), tt.wantChunks)
			}
			if resp.GetTotalSources() != tt.wantDistinct {
				t.Errorf("total sources = %d, want %d", resp.GetTotalSources(), tt.wantDistinct)
			}
			for i, src := range resp.GetSources() {
				if src.GetSourceUri() != tt.rag.sources[i].SourceURI {
					t.Errorf("source[%d] uri = %q, want %q", i, src.GetSourceUri(), tt.rag.sources[i].SourceURI)
				}
			}
		})
	}
}

func TestServerDeleteSource(t *testing.T) {
	tests := []struct {
		name        string
		rag         fakeRag
		wantDeleted int32
		wantErr     bool
	}{
		{name: "reports how many chunks went", rag: fakeRag{deleted: 7}, wantDeleted: 7},
		{name: "absent source deletes nothing", rag: fakeRag{deleted: 0}, wantDeleted: 0},
		{name: "error propagates", rag: fakeRag{err: errors.New("boom")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: tt.rag}
			resp, err := s.DeleteSource(context.Background(),
				&orchdv1.DeleteSourceRequest{SourceUri: "a://1", Confirm: true})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && resp.GetDeleted() != tt.wantDeleted {
				t.Errorf("deleted = %d, want %d", resp.GetDeleted(), tt.wantDeleted)
			}
		})
	}
}

func TestServerListTasks(t *testing.T) {
	tasks := &fakeTasks{list: []queue.Task{
		{ID: "t1", Agent: "dev", Status: "pending", Body: "b"},
		{ID: "t2", Agent: "ops", Status: "complete", Body: "c", Result: "done"},
	}}
	s := &server{start: time.Now(), rag: fakeRag{}, tasks: tasks}

	resp, err := s.ListTasks(context.Background(), &orchdv1.ListTasksRequest{
		Status: "pending", Agent: "dev", Limit: 5,
	})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(resp.GetTasks()) != 2 {
		t.Fatalf("tasks = %d, want 2", len(resp.GetTasks()))
	}
	if resp.GetTasks()[1].GetResult() != "done" {
		t.Errorf("result not carried through: %+v", resp.GetTasks()[1])
	}
	// The filter must reach the store rather than being silently dropped.
	if tasks.gotFilter.Status != "pending" || tasks.gotFilter.Agent != "dev" || tasks.gotFilter.Limit != 5 {
		t.Errorf("filter = %+v, want status=pending agent=dev limit=5", tasks.gotFilter)
	}
}

func TestServerListTasksErrors(t *testing.T) {
	t.Run("store error propagates", func(t *testing.T) {
		s := &server{start: time.Now(), rag: fakeRag{}, tasks: &fakeTasks{err: errors.New("db down")}}
		if _, err := s.ListTasks(context.Background(), &orchdv1.ListTasksRequest{}); err == nil {
			t.Fatal("expected an error")
		}
	})
	// A nil ledger must be a clean Unavailable rather than a nil-pointer panic.
	t.Run("no ledger reports unavailable", func(t *testing.T) {
		s := &server{start: time.Now(), rag: fakeRag{}}
		_, err := s.ListTasks(context.Background(), &orchdv1.ListTasksRequest{})
		if grpcstatus.Code(err) != codes.Unavailable {
			t.Fatalf("code = %v, want Unavailable", grpcstatus.Code(err))
		}
	})
}

func TestServerGetTask(t *testing.T) {
	tests := []struct {
		name      string
		tasks     *fakeTasks
		wantFound bool
		wantErr   bool
	}{
		{
			name:      "found",
			tasks:     &fakeTasks{one: queue.Task{ID: "t1", Status: "complete"}, found: true},
			wantFound: true,
		},
		{name: "missing id is not an error", tasks: &fakeTasks{found: false}, wantFound: false},
		{name: "store error propagates", tasks: &fakeTasks{err: errors.New("db down")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{start: time.Now(), rag: fakeRag{}, tasks: tt.tasks}
			resp, err := s.GetTask(context.Background(), &orchdv1.GetTaskRequest{Id: "t1"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if resp.GetFound() != tt.wantFound {
				t.Errorf("found = %v, want %v", resp.GetFound(), tt.wantFound)
			}
			if tt.wantFound && resp.GetTask().GetId() != "t1" {
				t.Errorf("task id = %q, want t1", resp.GetTask().GetId())
			}
			if !tt.wantFound && resp.GetTask() != nil {
				t.Errorf("a not-found response should carry no task, got %+v", resp.GetTask())
			}
		})
	}
}

func TestServerGetTaskWithoutALedger(t *testing.T) {
	s := &server{start: time.Now(), rag: fakeRag{}}
	_, err := s.GetTask(context.Background(), &orchdv1.GetTaskRequest{Id: "t1"})
	if grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", grpcstatus.Code(err))
	}
}

// The gate lives in the daemon, not the CLI: a client that simply omits confirm
// must be refused, or the CLI's --yes protects nobody but the CLI.
func TestServerDeleteSourceRequiresConfirmation(t *testing.T) {
	tests := []struct {
		name     string
		req      *orchdv1.DeleteSourceRequest
		wantCode codes.Code
	}{
		{
			name:     "unconfirmed delete is refused",
			req:      &orchdv1.DeleteSourceRequest{SourceUri: "a://1"},
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "empty uri is refused even when confirmed",
			req:      &orchdv1.DeleteSourceRequest{SourceUri: "", Confirm: true},
			wantCode: codes.InvalidArgument,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rag := fakeRag{deleted: 99}
			s := &server{start: time.Now(), rag: rag}
			_, err := s.DeleteSource(context.Background(), tt.req)
			if grpcstatus.Code(err) != tt.wantCode {
				t.Fatalf("code = %v, want %v (err: %v)", grpcstatus.Code(err), tt.wantCode, err)
			}
		})
	}
}
