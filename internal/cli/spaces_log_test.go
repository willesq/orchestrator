package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// logFake is what fakeClient's VerifyToolLog answers and records.
var logFake struct {
	req *orchdv1.VerifyToolLogRequest
	rep *orchdv1.VerifyToolLogResponse
	err error
}

func (f *fakeClient) VerifyToolLog(_ context.Context, req *orchdv1.VerifyToolLogRequest, _ ...grpc.CallOption) (*orchdv1.VerifyToolLogResponse, error) {
	logFake.req = req
	return logFake.rep, logFake.err
}

func runLogVerify(t *testing.T, rep *orchdv1.VerifyToolLogResponse, err error, owner string) (string, error) {
	t.Helper()
	logFake.req, logFake.rep, logFake.err = nil, rep, err
	t.Cleanup(func() { logFake.req, logFake.rep, logFake.err = nil, nil, nil })
	out, runErr := run(factory(&fakeClient{}, nil), "", "spaces", "log", "verify", owner)
	if logFake.req == nil || logFake.req.GetOwner() != owner {
		t.Errorf("VerifyToolLog request = %v, want owner %q", logFake.req, owner)
	}
	return out, runErr
}

func TestSpacesLogVerify(t *testing.T) {
	tests := []struct {
		name    string
		rep     *orchdv1.VerifyToolLogResponse
		want    []string
		wantErr bool
	}{
		{"intact", &orchdv1.VerifyToolLogResponse{Records: 12, Anchors: 1, Ends: 1},
			[]string{"sess-1: 12 record(s), 1 chain head(s) anchored at SessionEnd", "chain intact"}, false},
		{"gaps", &orchdv1.VerifyToolLogResponse{Records: 5, Gaps: 2, Dropped: 7},
			[]string{"2 gap record(s): 7 event(s) were dropped", "chain intact"}, false},
		{"empty", &orchdv1.VerifyToolLogResponse{}, []string{"sess-1: no records"}, false},
		{"broken", &orchdv1.VerifyToolLogResponse{Records: 4, Problems: []*orchdv1.ToolLogProblem{
			{Seq: 3, Detail: "content does not match its hash (modified or moved)"},
			{Seq: 5, Detail: "record 4 before it is missing"}}},
			[]string{"chain broken: 2 problem(s)", "record 3: content does not match", "record 5: record 4 before it is missing"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runLogVerify(t, tt.rep, nil, "sess-1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v\n%s", err, tt.wantErr, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
	if _, err := runLogVerify(t, nil, errors.New("only the operator can read the command log"), "task:task-1"); err == nil ||
		!strings.Contains(err.Error(), "operator") {
		t.Errorf("refusal = %v", err)
	}
}
