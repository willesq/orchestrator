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
	saved  *AgentTaskInput
	result string
	err    error
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
