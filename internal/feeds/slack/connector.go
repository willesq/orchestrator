// Package slack is the Slack connector (SP-061) for the chat connector
// framework in internal/feeds. It syncs a workspace's DMs and group DMs,
// then its allowlisted private channels, then its allowlisted public ones,
// each from a watermark kept per conversation: a day's messages outside
// threads are one record (slack://<team>/<channel>/<YYYY-MM-DD>) and a
// thread is one record under its ts (slack://<team>/<channel>/<ts>), written
// again under the same URI when it grows. A record whose text did not change
// is not sent again (and the store skips an unchanged write by URI and
// content hash).
//
// Credentials: the Slack API is called only from the connector process
// (Main, run as orchd's own executable with the argument Command), which
// reads the token from the profile directory the user wrote it to. orchd
// passes that process the directory's path, never reads inside it, and gets
// back records and watermarks only.
package slack

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/feeds"
	"github.com/willesq/orchestrator/internal/logs"
)

// Runner sends a Request to a connector process and hands each Line of its
// answer to out, stopping at the first error out returns.
type Runner func(ctx context.Context, req Request, out func(Line) error) error

// InProcess serves requests with f in this process (tests).
func InProcess(f Fetcher) Runner {
	return func(ctx context.Context, req Request, out func(Line) error) error {
		return f.Serve(ctx, req, out)
	}
}

// maxStderr caps what is kept of a connector process's stderr.
const maxStderr = 4096

// Process runs the connector process as path with args: the request goes to
// its stdin and its stdout is read line by line.
func Process(path string, args ...string) Runner {
	return func(ctx context.Context, req Request, out func(Line) error) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		in, err := json.Marshal(req)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Stdin = bytes.NewReader(in)
		stderr := &capped{max: maxStderr}
		cmd.Stderr = stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start the slack connector: %w", err)
		}
		reported, err := readLines(stdout, out)
		if err != nil {
			cancel()
			_ = cmd.Wait()
			return err
		}
		werr := cmd.Wait()
		switch {
		case reported != "":
			return errors.New(logs.Clean(reported))
		case werr != nil:
			return fmt.Errorf("the slack connector failed: %v: %s", werr, logs.Clean(stderr.String()))
		}
		return nil
	}
}

// maxLine caps one line of the connector process's answer. A record's text
// is capped near maxRecordText, and JSON escaping at most sextuples it, so
// a longer line is not one.
const maxLine = 8 * maxRecordText

// readLines decodes r's lines into out, and returns the error line the
// process reported, if any.
func readLines(r io.Reader, out func(Line) error) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	reported := ""
	for sc.Scan() {
		var l Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return "", errors.New("the slack connector's answer is not JSON lines")
		}
		if l.Error != "" {
			reported = l.Error
			continue
		}
		if err := out(l); err != nil {
			return "", err
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read the slack connector's answer: %w", err)
	}
	return reported, nil
}

// capped keeps the first max bytes written to it.
type capped struct {
	max int
	buf bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

// StateStore keeps a workspace's watermarks between runs.
type StateStore interface {
	Load() (State, error)
	Save(State) error
}

// FileState keeps watermarks in one JSON file, written atomically and
// readable by the user alone. It holds IDs, timestamps and hashes, never
// content.
type FileState struct{ Path string }

// Load reads the state; a missing file is an empty state.
func (s FileState) Load() (State, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("slack state %s: %w", s.Path, err)
	}
	return st, nil
}

// Save writes the state.
func (s FileState) Save(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".slack-state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Connector is the Slack connector as orchd runs it: a feeds.Connector for
// one workspace. It asks the connector process for what the allowlist names,
// hands the records to feeds.Sync, and saves a conversation's watermark only
// after every record of it was stored.
type Connector struct {
	// Tenant is the workspace's team ID, e.g. T0123.
	Tenant string
	// Profile is the profile directory the user wrote the token to.
	Profile string
	// Allow is the operator's allowlist; only what it names for this
	// workspace is read.
	Allow feeds.Allowlist
	State StateStore
	// Run reaches the connector process.
	Run Runner
	// History and Lookback are as in Request.
	History, Lookback time.Duration
}

// System is Slack.
func (c *Connector) System() feeds.System { return feeds.Slack }

// ErrForeignRecord stops a run whose connector process reported a record of
// another workspace or a conversation it was not asked for.
var ErrForeignRecord = errors.New("the slack connector reported a record it was not asked for")

// Read syncs the workspace once.
func (c *Connector) Read(ctx context.Context, emit func(feeds.Record) error) error {
	if c.Run == nil || c.State == nil {
		return errors.New("slack connector: no connector process or state store")
	}
	dms, containers := c.Allow.For(string(feeds.Slack) + ":" + c.Tenant)
	if !dms && len(containers) == 0 {
		return nil
	}
	st, err := c.State.Load()
	if err != nil {
		return err
	}
	// Reason: a conversation taken off the allowlist is not read again; its
	// watermark is dropped, so adding it back syncs it from the start.
	kept := map[string]ChannelState{}
	for id, cs := range st.Channels {
		if slices.Contains(containers, id) || (dms && cs.Kind.IsDirectMessage()) {
			kept[id] = cs
		}
	}
	st.Channels = kept
	req := Request{Op: OpFetch, Profile: c.Profile, Tenant: c.Tenant, DirectMessages: dms,
		Containers: containers, State: st, History: c.History, Lookback: c.Lookback}
	return c.Run(ctx, req, func(l Line) error {
		switch {
		case l.Record != nil:
			r := *l.Record
			asked := slices.Contains(containers, r.Container) || (dms && r.Kind.IsDirectMessage())
			if !strings.EqualFold(r.Tenant, c.Tenant) || r.System != feeds.Slack || !asked {
				return ErrForeignRecord
			}
			return emit(r)
		case l.Channel != nil:
			cs := l.Channel.State
			asked := slices.Contains(containers, l.Channel.ID) || (dms && cs.Kind.IsDirectMessage())
			if !feeds.ValidID(l.Channel.ID) || !asked {
				return ErrForeignRecord
			}
			if st.Channels == nil {
				st.Channels = map[string]ChannelState{}
			}
			st.Channels[l.Channel.ID] = l.Channel.State
			return c.State.Save(st)
		}
		return nil
	})
}

// Check asks the connector process whom the profile is logged in to:
// metadata only, never the token.
func Check(ctx context.Context, run Runner, profile string) (Status, error) {
	var st *Status
	err := run(ctx, Request{Op: OpStatus, Profile: profile}, func(l Line) error {
		if l.Status != nil {
			st = l.Status
		}
		return nil
	})
	if err != nil {
		return Status{}, err
	}
	if st == nil {
		return Status{}, errors.New("the slack connector reported no status")
	}
	return *st, nil
}
