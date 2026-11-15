package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/logs"
)

// The connector process. orchd runs it with a profile directory the user
// logged in to themselves and one Request on stdin; it answers with Lines
// on stdout. The token is read from the profile inside this process and
// sent only to Slack: no Line, error or log carries it, and orchd never
// sees it (D-59, the per-space gh profile pattern).

// The operations a Request asks for.
const (
	// OpFetch reads the requested conversations from their watermarks.
	OpFetch = "fetch"
	// OpStatus reports whom the profile's token belongs to: metadata only.
	OpStatus = "status"
)

// Request is what orchd asks the connector process.
type Request struct {
	Op string `json:"op"`
	// Profile is the profile directory's path. The process reads the
	// token file in it; orchd never does.
	Profile string `json:"profile"`
	// Tenant is the workspace's team ID the profile must be logged in to.
	Tenant string `json:"tenant"`
	// DirectMessages reads the user's DMs and group DMs.
	DirectMessages bool `json:"direct_messages,omitempty"`
	// Containers are the channel IDs read.
	Containers []string `json:"containers,omitempty"`
	State      State    `json:"state"`
	// History is how far back a conversation's first sync reads; 0 is
	// DefaultHistory.
	History time.Duration `json:"history,omitempty"`
	// Lookback is how long a thread is watched for new replies; 0 is
	// DefaultLookback.
	Lookback time.Duration `json:"lookback,omitempty"`
}

// Line is one line of the connector process's answer: exactly one field is
// set.
type Line struct {
	Record  *feeds.Record `json:"record,omitempty"`
	Channel *ChannelDone  `json:"channel,omitempty"`
	Status  *Status       `json:"status,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// ChannelDone ends one conversation: every record of it was handed over,
// and State is its new watermark.
type ChannelDone struct {
	ID    string       `json:"id"`
	State ChannelState `json:"state"`
}

// Status is whom a profile's token belongs to, from auth.test.
type Status struct {
	Team   string `json:"team"`
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
}

// TokenFile is the file in a profile directory that holds its token. The
// user writes it themselves (mode 0600); orch never writes or prints it.
const TokenFile = "token"

// ErrProfile refuses a profile directory or token file orch will not use.
var ErrProfile = errors.New("slack profile not usable")

// ErrWrongWorkspace refuses a profile logged in to another workspace than
// the tenant asked for. Reason: a record's tenant decides its space, so
// content must come from the workspace the request names.
var ErrWrongWorkspace = errors.New("the slack profile is logged in to another workspace")

// tokenPattern is a Slack token's shape; anything else in the token file is
// refused without being repeated.
var tokenPattern = regexp.MustCompile(`^xox[a-z](\.xox[a-z])?-[A-Za-z0-9-]{8,500}$`)

// Fetcher serves requests in the connector process.
type Fetcher struct {
	// BaseURL is Slack's Web API; "" is DefaultBaseURL. Tests point it at
	// a fake API.
	BaseURL string
	// HTTP is the client calls go through; nil is a client with a timeout.
	HTTP *http.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Sleep waits between rate-limited calls; nil waits for real.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Serve answers req through out. Nothing it hands to out or returns holds
// the token.
func (f Fetcher) Serve(ctx context.Context, req Request, out func(Line) error) error {
	if req.Op != OpFetch && req.Op != OpStatus {
		return fmt.Errorf("unknown operation %q", short(req.Op))
	}
	token, err := readToken(req.Profile)
	if err != nil {
		return err
	}
	scrub := func(s string) string { return strings.ReplaceAll(s, token, "[REDACTED]") }
	err = f.serve(ctx, req, token, func(l Line) error {
		if l.Record != nil {
			r := *l.Record
			r.Text = scrub(r.Text)
			l.Record = &r
		}
		return out(l)
	})
	if err != nil {
		return errors.New(scrub(err.Error()))
	}
	return nil
}

func (f Fetcher) serve(ctx context.Context, req Request, token string, out func(Line) error) error {
	c := &client{base: f.BaseURL, token: token, http: f.HTTP, sleep: f.Sleep}
	if c.base == "" {
		c.base = DefaultBaseURL
	}
	if c.http == nil {
		// Reason: a redirect would carry the Authorization header to
		// wherever it points on the same host; Slack's API does not
		// redirect, so none is followed.
		c.http = &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	id, err := c.authTest(ctx)
	if err != nil {
		return err
	}
	if req.Op == OpStatus {
		if !feeds.ValidID(id.TeamID) || !feeds.ValidID(id.UserID) {
			return errors.New("slack auth.test: the team or user ID is not a stable ID")
		}
		return out(Line{Status: &Status{Team: short(logs.Clean(id.Team)), TeamID: id.TeamID, UserID: id.UserID}})
	}
	if id.TeamID == "" || !strings.EqualFold(id.TeamID, req.Tenant) {
		return fmt.Errorf("%w (%q, not %q)", ErrWrongWorkspace, short(id.TeamID), short(req.Tenant))
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	ft := &fetcher{api: c, req: req, self: id.UserID, now: now().UTC(), out: out}
	return ft.run(ctx)
}

// short is v cut to 64 bytes, for a value read from outside.
func short(v string) string {
	if len(v) > 64 {
		return strings.ToValidUTF8(v[:64], "") + "…"
	}
	return v
}

// maxToken caps the token file.
const maxToken = 4096

// readToken reads the profile's token. The profile must pass the gh
// profile checks (an absolute directory under the home directory, the
// user's alone, in folders no one else can change), and the token file must
// be a regular file of the user's that no one else can open. The checks and
// the read use the profile's resolved path and the opened file itself, so a
// link swapped in between cannot substitute another file. No error repeats
// the file's content.
func readToken(profile string) (string, error) {
	notUsable := func(err error) error {
		return fmt.Errorf("%w: %s", ErrProfile, strings.TrimPrefix(err.Error(), github.ErrProfileDir.Error()+": "))
	}
	if err := github.CheckProfileDir(profile); err != nil {
		return "", notUsable(err)
	}
	real, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return "", fmt.Errorf("%w: %s cannot be read", ErrProfile, profile)
	}
	if err := github.CheckProfileDir(real); err != nil {
		return "", notUsable(err)
	}
	path := filepath.Join(real, TokenFile)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("%w: %s has no %s file; write the workspace's token to it yourself (mode 0600)", ErrProfile, profile, TokenFile)
	case errors.Is(err, syscall.ELOOP):
		return "", fmt.Errorf("%w: %s is not a regular file", ErrProfile, path)
	case err != nil:
		return "", fmt.Errorf("%w: %s cannot be read", ErrProfile, path)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %s cannot be read", ErrProfile, path)
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("%w: %s is not a regular file", ErrProfile, path)
	case fi.Mode().Perm()&0o077 != 0:
		return "", fmt.Errorf("%w: other users can open %s; make it yours alone (chmod 600)", ErrProfile, path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("%w: %s is owned by another user", ErrProfile, path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %s cannot be read", ErrProfile, path)
	case len(b) > maxToken:
		return "", fmt.Errorf("%w: %s is not a token", ErrProfile, path)
	}
	token := strings.TrimSpace(string(b))
	if !tokenPattern.MatchString(token) {
		return "", fmt.Errorf("%w: %s does not hold a Slack token", ErrProfile, path)
	}
	return token, nil
}

// maxRequest caps the request the connector process reads.
const maxRequest = 64 << 20

// Main is the connector process: it reads one Request from stdin, answers
// on stdout and returns the exit code. orchd starts it as its own
// executable with the argument Command.
func Main(ctx context.Context, stdin io.Reader, stdout io.Writer) int {
	return Fetcher{}.Main(ctx, stdin, stdout)
}

// Main is the connector process with f's settings.
func (f Fetcher) Main(ctx context.Context, stdin io.Reader, stdout io.Writer) int {
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	var req Request
	if err := json.NewDecoder(io.LimitReader(stdin, maxRequest)).Decode(&req); err != nil {
		_ = enc.Encode(Line{Error: "the request is not JSON"})
		return 2
	}
	if err := f.Serve(ctx, req, func(l Line) error { return enc.Encode(l) }); err != nil {
		_ = enc.Encode(Line{Error: err.Error()})
		return 1
	}
	return 0
}

// Command is the argument that makes orchd's executable the Slack
// connector process.
const Command = "slack-connector"
