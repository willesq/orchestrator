package orchdclient

import "testing"

func TestDial(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "returns a lazy client and cleanup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_SOCKET", "/tmp/orch-nonexistent-test.sock")
			client, cleanup, err := Dial()
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			if client == nil {
				t.Fatalf("nil client")
			}
			cleanup()
		})
	}
}
