package sources

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	day := int64(86400)
	tests := []struct {
		name    string
		in      Source
		want    Source
		wantErr string
	}{
		{
			name: "web URL, trimmed, owner cleared",
			in:   Source{Kind: KindWeb, URI: " https://go.dev/doc/ ", Owner: "x", RefreshSecs: day, Scope: " work ", Tags: []string{"go", " go ", "", "docs"}},
			want: Source{Kind: KindWeb, URI: "https://go.dev/doc/", RefreshSecs: day, Scope: "work", Tags: []string{"go", "docs"}},
		},
		{name: "web needs http or https", in: Source{Kind: KindWeb, URI: "file:///etc/passwd", RefreshSecs: day}, wantErr: "http or https"},
		{name: "web needs a host", in: Source{Kind: KindWeb, URI: "https:///x", RefreshSecs: day}, wantErr: "no host"},
		{name: "web without credentials", in: Source{Kind: KindWeb, URI: "https://me:pw@x.dev/", RefreshSecs: day}, wantErr: "credentials"},
		{name: "web URL that does not parse", in: Source{Kind: KindWeb, URI: "https://x.dev/%zz", RefreshSecs: day}, wantErr: "invalid URL"},
		{
			name: "repo path is cleaned",
			in:   Source{Kind: KindRepo, URI: "/Users/me/code/./app/", RefreshSecs: day},
			want: Source{Kind: KindRepo, URI: "/Users/me/code/app", RefreshSecs: day},
		},
		{name: "repo path must be absolute", in: Source{Kind: KindRepo, URI: "code/app", RefreshSecs: day}, wantErr: "absolute"},
		{
			name: "owner sets Owner",
			in:   Source{Kind: KindOwner, URI: "willesq", RefreshSecs: day},
			want: Source{Kind: KindOwner, URI: "willesq", Owner: "willesq", RefreshSecs: day},
		},
		{name: "owner cannot climb out of the roots", in: Source{Kind: KindOwner, URI: "../../etc", RefreshSecs: day}, wantErr: "directory name"},
		{name: "owner cannot be a path", in: Source{Kind: KindOwner, URI: "a/b", RefreshSecs: day}, wantErr: "directory name"},
		{name: "owner local is reserved", in: Source{Kind: KindOwner, URI: "local", RefreshSecs: day}, wantErr: "reserved"},
		{name: "reserved tag prefix", in: Source{Kind: KindOwner, URI: "o", RefreshSecs: day, Tags: []string{"repo:x"}}, wantErr: "reserved prefix"},
		{name: "unknown kind", in: Source{Kind: "ftp", URI: "x", RefreshSecs: day}, wantErr: "kind must be"},
		{name: "refresh too short", in: Source{Kind: KindOwner, URI: "o", RefreshSecs: 30}, wantErr: "refresh interval"},
		{name: "refresh too long", in: Source{Kind: KindOwner, URI: "o", RefreshSecs: 400 * day}, wantErr: "refresh interval"},
		{name: "overlong URI", in: Source{Kind: KindRepo, URI: "/" + strings.Repeat("a", maxURILen), RefreshSecs: day}, wantErr: "longer than"},
		{name: "scope too long", in: Source{Kind: KindOwner, URI: "o", RefreshSecs: day, Scope: strings.Repeat("s", 65)}, wantErr: "scope"},
		{name: "scope with control characters", in: Source{Kind: KindOwner, URI: "o", RefreshSecs: day, Scope: "a‮b"}, wantErr: "control"},
		{name: "tag with a comma", in: Source{Kind: KindOwner, URI: "o", RefreshSecs: day, Tags: []string{"a,b"}}, wantErr: "comma"},
		{name: "tag too long", in: Source{Kind: KindOwner, URI: "o", RefreshSecs: day, Tags: []string{strings.Repeat("t", 65)}}, wantErr: "tag"},
		{name: "too many tags", in: Source{Kind: KindOwner, URI: "o", RefreshSecs: day, Tags: manyTags(33)}, wantErr: "at most"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.in
			err := Validate(&s)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if s.Kind != tt.want.Kind || s.URI != tt.want.URI || s.Owner != tt.want.Owner || s.Scope != tt.want.Scope ||
				s.RefreshSecs != tt.want.RefreshSecs || strings.Join(s.Tags, ",") != strings.Join(tt.want.Tags, ",") {
				t.Errorf("normalized = %+v, want %+v", s, tt.want)
			}
		})
	}
}

func manyTags(n int) []string {
	var out []string
	for i := range n {
		out = append(out, strings.Repeat("t", i+1))
	}
	return out
}
