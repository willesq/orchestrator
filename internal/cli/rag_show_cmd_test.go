package cli

import (
	"errors"
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestRAGShow(t *testing.T) {
	tests := []struct {
		name    string
		doc     *orchdv1.Document
		err     error
		wantSub []string
		wantNot []string
		wantErr string
	}{
		{
			name: "prints the rebuilt text with its origin",
			doc: &orchdv1.Document{SourceUri: "repo://o/r/README.md", SourceType: "repo_doc", Chunks: 2, Text: "# Title\n\nBody",
				WebUrl: "https://github.com/o/r/blob/abc/README.md"},
			wantSub: []string{"repo://o/r/README.md (repo_doc, 2 chunks)", "original: https://github.com/o/r/blob/abc/README.md", "# Title\n\nBody"},
			wantNot: []string{"note:"},
		},
		{
			name:    "neutralizes terminal escapes and bidi overrides",
			doc:     &orchdv1.Document{SourceUri: "u\x1b]0;x\x07", SourceType: "web", Text: "a\x1b[2Jb\u202ec\n\td\xff"},
			wantSub: []string{"a\uFFFD[2Jb\uFFFDc\n\td\uFFFD"},
			wantNot: []string{"\x1b", "\u202e", "\x07"},
		},
		{
			name:    "flags a best-effort rebuild",
			doc:     &orchdv1.Document{SourceUri: "u", Malformed: true, Text: "t"},
			wantSub: []string{"chunk order is incomplete", "t"},
		},
		{
			name:    "says when it is too large",
			doc:     &orchdv1.Document{SourceUri: "u", Oversize: true},
			wantSub: []string{"too large"},
		},
		{name: "reports a failure", err: errors.New("not found: nothing is indexed"), wantErr: "nothing is indexed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeClient{document: tt.doc, err: tt.err}
			out, err := run(factory(fc, nil), "", "rag", "show", "repo://o/r/README.md")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("rag show: %v", err)
			}
			if fc.docReq.GetSourceUri() != "repo://o/r/README.md" {
				t.Errorf("request = %v", fc.docReq)
			}
			for _, s := range tt.wantSub {
				if !strings.Contains(out, s) {
					t.Errorf("output missing %q:\n%s", s, out)
				}
			}
			for _, s := range tt.wantNot {
				if strings.Contains(out, s) {
					t.Errorf("output has %q:\n%s", s, out)
				}
			}
		})
	}
}
