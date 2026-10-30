package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// taskControl records RetryTask and CancelTask calls on fakeClient.
var taskControl struct {
	retried, cancelled []string
	err                error
}

func (f *fakeClient) RetryTask(_ context.Context, req *orchdv1.RetryTaskRequest, _ ...grpc.CallOption) (*orchdv1.RetryTaskResponse, error) {
	if taskControl.err != nil {
		return nil, taskControl.err
	}
	taskControl.retried = append(taskControl.retried, req.GetId())
	return &orchdv1.RetryTaskResponse{RunId: req.GetId() + "#retry-1"}, nil
}

func (f *fakeClient) CancelTask(_ context.Context, req *orchdv1.CancelTaskRequest, _ ...grpc.CallOption) (*orchdv1.CancelTaskResponse, error) {
	if taskControl.err != nil {
		return nil, taskControl.err
	}
	taskControl.cancelled = append(taskControl.cancelled, req.GetId())
	return &orchdv1.CancelTaskResponse{Outcome: "stopping its running session"}, nil
}

func TestTaskRetryAndCancelFlags(t *testing.T) {
	taskControl.retried, taskControl.cancelled, taskControl.err = nil, nil, nil
	t.Cleanup(func() { taskControl.retried, taskControl.cancelled, taskControl.err = nil, nil, nil })
	cf := factory(&fakeClient{}, nil)
	out, err := run(cf, "", "task", "task-1", "--retry")
	if err != nil || !strings.Contains(out, "retrying task-1 as run task-1#retry-1") {
		t.Fatalf("--retry = %q, %v", out, err)
	}
	out, err = run(cf, "", "task", "task-2", "--cancel")
	if err != nil || !strings.Contains(out, "cancelled task-2: stopping its running session") {
		t.Fatalf("--cancel = %q, %v", out, err)
	}
	if len(taskControl.retried) != 1 || len(taskControl.cancelled) != 1 {
		t.Errorf("calls = %+v", taskControl)
	}
	if _, err := run(cf, "", "task", "task-3", "--retry", "--cancel"); err == nil {
		t.Error("--retry with --cancel was accepted")
	}
	if _, err := run(cf, "", "task", "task-3", "--rate", "good", "--retry"); err == nil {
		t.Error("--rate with --retry was accepted")
	}
	taskControl.err = errors.New("task-4 is in_progress and its run is still active")
	if _, err := run(cf, "", "task", "task-4", "--retry"); err == nil {
		t.Error("a refused retry did not surface")
	}
	if _, err := run(cf, "", "task", "task-4", "--cancel"); err == nil {
		t.Error("a refused cancel did not surface")
	}
}

func TestTaskShowsLiveness(t *testing.T) {
	cf := factory(&fakeClient{getTask: &orchdv1.GetTaskResponse{Found: true, Task: &orchdv1.Task{
		Id: "task-5", Status: "in_progress", Agent: "dev", Body: "b",
		Run: &orchdv1.TaskRun{State: "stalled", SessionId: "sid"}}}}, nil)
	out, err := run(cf, "", "task", "task-5")
	if err != nil || !strings.Contains(out, "stalled") || !strings.Contains(out, "session: sid") {
		t.Errorf("task output = %q, %v", out, err)
	}
	list := factory(&fakeClient{listTasks: &orchdv1.ListTasksResponse{Tasks: []*orchdv1.Task{{
		Id: "task-5", Status: "in_progress", Agent: "dev", Run: &orchdv1.TaskRun{State: "stalled"}}}}}, nil)
	if out, err := run(list, "", "tasks"); err != nil || !strings.Contains(out, "in_progress ⚠ stalled") {
		t.Errorf("tasks output = %q, %v", out, err)
	}
}
