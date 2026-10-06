package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantCount int
		wantGone  string
	}{
		{name: "clean text untouched", in: "hello world", wantCount: 0},
		{name: "github token", in: "token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", wantCount: 1, wantGone: "ghp_"},
		{name: "aws key", in: "key AKIAIOSFODNN7EXAMPLE here", wantCount: 1, wantGone: "AKIA"},
		{name: "openai key", in: "sk-abcdefghijklmnopqrstuvwxyz012345", wantCount: 1, wantGone: "sk-abc"},
		{name: "openai project key", in: "key sk-proj-" + strings.Repeat("Ab3_-x", 8) + " end", wantCount: 1, wantGone: "sk-proj-"},
		{name: "openai service account key", in: "sk-svcacct-" + strings.Repeat("Zq9", 10), wantCount: 1, wantGone: "sk-svcacct-"},
		{name: "github fine-grained pat", in: "pat github_pat_" + strings.Repeat("A1b2C", 6) + "_" + strings.Repeat("x9", 20), wantCount: 1, wantGone: "github_pat_"},
		{name: "anthropic key not double counted", in: "sk-ant-" + strings.Repeat("k7", 15), wantCount: 1, wantGone: "sk-ant-"},
		{name: "private key block", in: "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----", wantCount: 1, wantGone: "BEGIN RSA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, n := Redact(tt.in)
			if n != tt.wantCount {
				t.Errorf("count = %d, want %d (out=%q)", n, tt.wantCount, out)
			}
			if tt.wantGone != "" && strings.Contains(out, tt.wantGone) {
				t.Errorf("secret not redacted: %q still in %q", tt.wantGone, out)
			}
			if tt.wantCount > 0 && !strings.Contains(out, "[REDACTED:") {
				t.Errorf("no redaction marker in %q", out)
			}
		})
	}
}
