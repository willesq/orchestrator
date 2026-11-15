package teams

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/rag"
)

// fakeToken stands for the Graph token the user's az login holds. Only the
// fake az below knows it; the test checks it never reaches orch.
const fakeToken = "eyJ0eXAiOiJKV1QifQ.eyJhdWQiOiJncmFwaCJ9.c2lnbmF0dXJlLW5vdC1yZWFs"

// fakeAz writes an az stand-in to a temporary directory. It checks that it
// runs with AZURE_CONFIG_DIR set to the profile, keeps a "token" in that
// profile, and answers by the URL it is asked for from responses (a URL
// substring, or an exact Graph URL, to a JSON body); "fail:<n>" exits n after printing the token
// to standard error, "login" says the profile is not logged in.
func fakeAz(t *testing.T, profile string, responses map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "msal_cache.json"), []byte(fakeToken), 0o600); err != nil {
		t.Fatal(err)
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString(`if [ "$AZURE_CONFIG_DIR" != "` + profile + `" ]; then echo "wrong profile $AZURE_CONFIG_DIR" >&2; exit 9; fi` + "\n")
	script.WriteString(`token=$(cat "$AZURE_CONFIG_DIR/msal_cache.json")` + "\n")
	script.WriteString(`args="$*"` + "\n")
	script.WriteString(`case "$args" in` + "\n")
	n := 0
	for k, body := range responses {
		pattern := "*'" + k + "'*)\n"
		if strings.HasPrefix(k, GraphBase) {
			// A recorded URL matches exactly, not as a prefix of a longer one.
			pattern = "*'" + k + " --output json')\n"
		}
		script.WriteString(pattern)
		switch {
		case strings.HasPrefix(body, "fail:"):
			script.WriteString(`    echo "ERROR: request failed, Authorization: Bearer $token" >&2; exit ` + strings.TrimPrefix(body, "fail:") + "\n")
		case body == "login":
			script.WriteString(`    echo "ERROR: Please run 'az login' to setup account. $token" >&2; exit 1` + "\n")
		default:
			n++
			f := filepath.Join(dir, fmt.Sprintf("r%d.json", n))
			if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			script.WriteString(`    cat '` + f + `'; exit 0` + "\n")
		}
		script.WriteString("    ;;\n")
	}
	script.WriteString("  *) echo \"no fake for $args\" >&2; exit 3;;\nesac\n")
	bin := filepath.Join(dir, "az")
	if err := os.WriteFile(bin, []byte(script.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestAzRestGet(t *testing.T) {
	profile := t.TempDir()
	t.Setenv("AZURE_CONFIG_DIR", "/somewhere/else")
	bin := fakeAz(t, profile, map[string]string{
		"rest --method get --url " + GraphBase + "/me/chats --output json": `{"value":[]}`,
		"/me/joinedTeams": "login",
		"/teams/x":        "fail:2",
		"account show":    `{"tenantId":"` + tenant + `","user":{"name":"alex@example.com","type":"user"}}`,
	})
	a := AzRest{Bin: bin, ConfigDir: profile}
	ctx := context.Background()
	if out, err := a.Get(ctx, GraphBase+"/me/chats"); err != nil || string(out) != `{"value":[]}` {
		t.Fatalf("Get = %q, %v", out, err)
	}
	if _, err := a.Get(ctx, GraphBase+"/me/joinedTeams"); !errors.Is(err, ErrNotLoggedIn) || strings.Contains(err.Error(), fakeToken) {
		t.Errorf("not logged in = %v", err)
	}
	_, err := a.Get(ctx, GraphBase+"/teams/x")
	if !errors.Is(err, ErrAz) || !strings.Contains(err.Error(), "status 2") || strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), "Bearer") {
		t.Errorf("failure = %v", err)
	}
	if _, err := a.Get(ctx, "https://evil.example/v1.0/me"); !errors.Is(err, ErrAz) {
		t.Errorf("non-Graph URL = %v", err)
	}
	if _, err := a.Get(ctx, "https://graph.microsoft.com.evil.example/v1.0/me"); !errors.Is(err, ErrAz) {
		t.Errorf("look-alike host = %v", err)
	}
	acct, err := a.Status(ctx)
	if err != nil || acct != (Account{Tenant: tenant, User: "alex@example.com"}) {
		t.Errorf("Status = %+v, %v", acct, err)
	}
	if _, err := (AzRest{Bin: bin}).Get(ctx, GraphBase+"/me/chats"); !errors.Is(err, ErrAz) {
		t.Errorf("no profile = %v", err)
	}
	if _, err := (AzRest{Bin: filepath.Join(t.TempDir(), "none"), ConfigDir: profile}).Get(ctx, GraphBase+"/me/chats"); !errors.Is(err, ErrAz) {
		t.Errorf("missing az = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.Get(cancelled, GraphBase+"/me/chats"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled = %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := (AzRest{ConfigDir: profile}).Status(ctx); !errors.Is(err, ErrAz) {
		t.Errorf("az not on PATH = %v", err)
	}
	big := t.TempDir()
	if err := os.WriteFile(filepath.Join(big, "az"), []byte("#!/bin/sh\n/usr/bin/head -c 4096 /dev/zero\n/usr/bin/head -c 100000 /dev/zero >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	defer func(n int) { maxOutput = n }(maxOutput)
	maxOutput = 1024
	if _, err := (AzRest{Bin: filepath.Join(big, "az"), ConfigDir: profile}).Get(ctx, GraphBase+"/me/chats"); !errors.Is(err, ErrAz) || !strings.Contains(err.Error(), "more than") {
		t.Errorf("an oversized answer = %v", err)
	}
	maxOutput = 8192
	if out, err := (AzRest{Bin: filepath.Join(big, "az"), ConfigDir: profile}).Get(ctx, GraphBase+"/me/chats"); err != nil || len(out) != 4096 {
		t.Errorf("long stderr: %d bytes, %v", len(out), err)
	}
	other := AzRest{Bin: fakeAz(t, profile, map[string]string{"account show": `{"user":{}}`}), ConfigDir: profile}
	if _, err := other.Status(ctx); !errors.Is(err, ErrAz) {
		t.Errorf("no tenant = %v", err)
	}
}

// TestNoTokenInLogsErrorsOrContent is SP-061.1.a2's contract for Teams: a
// full sync through az, with a request that fails while az prints its
// token, leaves no token value in orch's logs, errors, stored content or
// state.
func TestNoTokenInLogsErrorsOrContent(t *testing.T) {
	profile := t.TempDir()
	g := newFakeGraph(t)
	// Someone pasted the token into a chat too.
	g.post(general, "1731690000000", "", "u-kim", "my token is "+fakeToken, day1.Add(18*time.Hour))
	// Record what Graph answers each request of a sync, then serve those
	// answers through the fake az.
	responses := map[string]string{}
	recorder := GraphFunc(func(ctx context.Context, raw string) ([]byte, error) {
		body, err := g.Get(ctx, raw)
		if !strings.Contains(raw, group) {
			responses[raw] = string(body)
		}
		return body, err
	})
	rc := &Connector{Graph: recorder, Tenant: tenant, Allow: allowed()}
	if _, err := feeds.Sync(context.Background(), rc, rc.Allow, discard{}); err != nil {
		t.Fatal(err)
	}
	responses[group] = "fail:4"
	bin := fakeAz(t, profile, responses)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, _ := OpenState(statePath)
	c := &Connector{Graph: AzRest{Bin: bin, ConfigDir: profile}, Tenant: tenant, Allow: allowed(), State: st}
	s := newStore()
	_, err := feeds.Sync(context.Background(), c, c.Allow, s)
	if err == nil {
		t.Fatal("the failed request was not reported")
	}
	if len(s.docs) == 0 {
		t.Fatalf("nothing stored: %v", err)
	}
	state, _ := os.ReadFile(statePath)
	for where, text := range map[string]string{"error": err.Error(), "logs": logs.String(), "state": string(state)} {
		if strings.Contains(text, fakeToken) || strings.Contains(text, "Bearer") {
			t.Errorf("a token reached the %s: %q", where, text)
		}
	}
	for u, d := range s.docs {
		if strings.Contains(d, fakeToken) {
			t.Errorf("a token was stored under %s", u)
		}
	}
	if !strings.Contains(s.docs[uri(general, "1731690000000")], "[REDACTED") {
		t.Errorf("the pasted token was not redacted: %q", s.docs[uri(general, "1731690000000")])
	}
}

// TestNoCodeReadsAzureCredentials keeps this package from reading az's
// login: no code asks az for a token or opens its token cache.
func TestNoCodeReadsAzureCredentials(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"get-access-token", "msal_token_cache", "accessTokens", "msal_http_cache", "AZURE_ACCESS_TOKEN", "service_principal_entries"} {
			if bytes.Contains(b, []byte(bad)) {
				t.Errorf("%s mentions %s", f, bad)
			}
		}
	}
}

// discard stores nothing.
type discard struct{}

func (discard) IngestText(context.Context, string, rag.Meta) (int, error) { return 1, nil }
