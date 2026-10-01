package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cschleiden/go-workflows/tester"
	"github.com/stretchr/testify/mock"
)

type fakeStore struct {
	saved     *AgentTaskInput
	result    string
	err       error
	statuses  []string
	statusErr error
}

func (f *fakeStore) SetStatus(_ context.Context, _ string, status string) error {
	if f.statusErr != nil {
		return f.statusErr
	}
	f.statuses = append(f.statuses, status)
	return nil
}

func (f *fakeStore) SaveResult(_ context.Context, in AgentTaskInput, result string) error {
	if f.err != nil {
		return f.err
	}
	f.saved = &in
	f.result = result
	return nil
}

func TestProcessActivity(t *testing.T) {
	tests := []struct {
		name    string
		store   *fakeStore
		body    string
		want    string
		wantErr bool
	}{
		{name: "persists and returns result", store: &fakeStore{}, body: "do X", want: "processed: do X"},
		{name: "store error propagates", store: &fakeStore{err: errors.New("db down")}, body: "y", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetStore(tt.store)
			t.Cleanup(func() { SetStore(nil) })

			got, err := ProcessActivity(context.Background(), AgentTaskInput{ID: "id", Agent: "dev", Body: tt.body})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
			if tt.store.saved == nil {
				t.Errorf("store did not persist the task")
			}
		})
	}
}

func TestAgentTaskWorkflow(t *testing.T) {
	tests := []struct {
		name       string
		activityRV string
		want       string
	}{
		{name: "sleeps then processes", activityRV: "processed: hi", want: "processed: hi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wt := tester.NewWorkflowTester[string](AgentTask, tester.WithTestTimeout(5*time.Second))
			wt.OnActivity(StartActivity, mock.Anything, mock.Anything).Return("id", nil)
			wt.OnActivity(ProcessActivity, mock.Anything, mock.Anything).Return(tt.activityRV, nil)

			wt.Execute(context.Background(), AgentTaskInput{ID: "id", Agent: "dev", Body: "hi"})

			if !wt.WorkflowFinished() {
				t.Fatalf("workflow did not finish")
			}
			got, err := wt.WorkflowResult()
			if err != nil {
				t.Fatalf("workflow error: %v", err)
			}
			if got != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStartActivityMarksTheTaskInProgress(t *testing.T) {
	tests := []struct {
		name         string
		store        *fakeStore
		nilStore     bool
		wantStatuses []string
		wantErr      bool
	}{
		{name: "records in_progress", store: &fakeStore{}, wantStatuses: []string{"in_progress"}},
		{name: "store error propagates", store: &fakeStore{statusErr: errors.New("db down")}, wantErr: true},
		{name: "no store configured is a no-op", nilStore: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.nilStore {
				SetStore(nil)
			} else {
				SetStore(tt.store)
			}
			t.Cleanup(func() { SetStore(nil) })

			got, err := StartActivity(context.Background(), AgentTaskInput{ID: "id-1", Agent: "dev", Body: "b"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != "id-1" {
				t.Errorf("returned %q, want the task id", got)
			}
			if tt.nilStore {
				return
			}
			if len(tt.store.statuses) != len(tt.wantStatuses) {
				t.Fatalf("statuses = %v, want %v", tt.store.statuses, tt.wantStatuses)
			}
			for i, w := range tt.wantStatuses {
				if tt.store.statuses[i] != w {
					t.Errorf("status[%d] = %q, want %q", i, tt.store.statuses[i], w)
				}
			}
		})
	}
}
