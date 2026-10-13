package workflows

import (
	"context"
	"errors"
	"fmt"
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

func (f *fakeStore) SaveResult(_ context.Context, in AgentTaskInput, out Outcome) error {
	status, result := out.Status, out.Result
	if f.err != nil {
		return f.err
	}
	f.saved = &in
	f.statuses = append(f.statuses, status)
	f.result = result
	return nil
}

type fakeRunner struct {
	out Outcome
	err error
	got *AgentTaskInput
}

func (f *fakeRunner) Run(_ context.Context, in AgentTaskInput) (Outcome, error) {
	f.got = &in
	return f.out, f.err
}

func TestRunActivity(t *testing.T) {
	interrupted := fmt.Errorf("shutdown: %w", context.Canceled)
	tests := []struct {
		name         string
		runner       *fakeRunner
		store        *fakeStore
		want         string
		wantErr      error
		wantStatus   string
		wantNotified bool
	}{
		{
			name:   "records the run's status and result and notifies",
			runner: &fakeRunner{out: Outcome{Status: "complete", Result: "opened PR #7"}},
			store:  &fakeStore{}, want: "opened PR #7", wantStatus: "complete", wantNotified: true,
		},
		{
			name:   "a failed run is recorded, not returned as an error",
			runner: &fakeRunner{out: Outcome{Status: "failed", Result: "tests broke"}},
			store:  &fakeStore{}, want: "tests broke", wantStatus: "failed", wantNotified: true,
		},
		{
			name:   "an interrupted run is abandoned without touching the ledger",
			runner: &fakeRunner{err: interrupted},
			store:  &fakeStore{}, wantErr: context.Canceled,
		},
		{
			name:   "store error propagates",
			runner: &fakeRunner{out: Outcome{Status: "complete", Result: "x"}},
			store:  &fakeStore{err: errors.New("db down")}, wantErr: errors.New("db down"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var notified []Outcome
			SetStore(tt.store)
			SetRunner(tt.runner)
			SetNotifier(func(_ AgentTaskInput, o Outcome) { notified = append(notified, o) })
			t.Cleanup(func() { SetStore(nil); SetRunner(nil); SetNotifier(nil) })

			in := AgentTaskInput{ID: "id", Agent: "dev", Body: "b", Cwd: "/src"}
			got, err := RunActivity(context.Background(), in)
			if tt.wantErr != nil {
				if err == nil || (errors.Is(tt.wantErr, context.Canceled) && !errors.Is(err, context.Canceled)) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if len(notified) != 0 {
					t.Errorf("notified on error: %v", notified)
				}
				if errors.Is(tt.wantErr, context.Canceled) && tt.store.saved != nil {
					t.Errorf("interrupted run wrote the ledger")
				}
				return
			}
			if err != nil {
				t.Fatalf("RunActivity: %v", err)
			}
			if got != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
			if *tt.runner.got != in {
				t.Errorf("runner got %+v, want %+v", *tt.runner.got, in)
			}
			if len(tt.store.statuses) != 1 || tt.store.statuses[0] != tt.wantStatus {
				t.Errorf("saved statuses = %v, want [%s]", tt.store.statuses, tt.wantStatus)
			}
			if tt.wantNotified && len(notified) != 1 {
				t.Errorf("notified %d times, want 1", len(notified))
			}
		})
	}
}

func TestRunActivityWithoutRunner(t *testing.T) {
	SetRunner(nil)
	if _, err := RunActivity(context.Background(), AgentTaskInput{ID: "id"}); !errors.Is(err, errNoRunner) {
		t.Fatalf("err = %v, want errNoRunner", err)
	}
}

func TestAgentTaskWorkflow(t *testing.T) {
	tests := []struct {
		name    string
		runRV   string
		runErr  error
		want    string
		wantErr bool
	}{
		{name: "marks running then runs", runRV: "done", want: "done"},
		{name: "a failing run is attempted exactly once", runErr: errors.New("boom"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wt := tester.NewWorkflowTester[string](AgentTask, tester.WithTestTimeout(5*time.Second))
			wt.OnActivity(StartActivity, mock.Anything, mock.Anything).Return("id", nil)
			wt.OnActivity(RunActivity, mock.Anything, mock.Anything).Return(tt.runRV, tt.runErr).Once()

			wt.Execute(context.Background(), AgentTaskInput{ID: "id", Agent: "dev", Body: "hi"})

			if !wt.WorkflowFinished() {
				t.Fatalf("workflow did not finish")
			}
			got, err := wt.WorkflowResult()
			if (err != nil) != tt.wantErr {
				t.Fatalf("workflow error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
			wt.AssertExpectations(t)
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
