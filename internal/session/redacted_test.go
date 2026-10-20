package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRedacted(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	p := filepath.Join(t.TempDir(), "t.jsonl")
	body := `{"type":"user","message":{"role":"user","content":"use ` + secret + ` to deploy"}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseRedacted(p)
	if err != nil || len(msgs) != 1 || strings.Contains(msgs[0].Text, secret) || !strings.Contains(msgs[0].Text, "to deploy") {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	if _, err := ParseRedacted(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Fatal("a missing transcript parsed")
	}
}
