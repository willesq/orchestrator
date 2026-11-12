package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// maxPolicyFile is one byte over orchd's policy limit, so an oversized
// policy is refused with orchd's (or Parse's) message, not cut short.
const maxPolicyFile = 1<<20 + 1

func newSpacesReplayCmd(cf clientFactory) *cobra.Command {
	var policy, mode string
	var all bool
	cmd := &cobra.Command{
		Use:   "replay <session id | task:<task id> | process:<pid> | fixtures path>",
		Short: "Decide recorded tool calls again and list the verdicts that change",
		Long: `Replay runs recorded tool calls through the tool guard's decision function
again and lists every call whose verdict or rule differs from the one recorded,
so a policy change can be tried on real behavior before it is applied. It
changes nothing.

With a session ID, task:<task id> or process:<pid>, orchd replays that command
log. Each call is decided under the revision applied when it was recorded,
unless --policy names a saved revision number or a policy file. Operator only:
refused inside an agent session and inside tasks orchd runs.

With a path (a .jsonl file of recorded hook payloads, or a folder of them, such
as internal/hook/testdata/payloads), the payloads are replayed here, without
orchd, under the policy file --policy names. Each session is pinned to the
space of its first directory. Payloads carry no recorded verdict, so use --all
to list every call's verdict.

--mode judges in a guard mode (off, log, block-other-jobs, strict); by default
orchd's current guard switch, or block-other-jobs for payload files. In off and
log the verdict is the one the guard would give in block-other-jobs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			out := c.OutOrStdout()
			if isFixturePath(args[0]) {
				return replayFixtures(out, args[0], policy, mode, all)
			}
			req := &orchdv1.ReplaySpacesRequest{Owner: args[0], Mode: mode, All: all}
			if err := setReplayPolicy(req, policy); err != nil {
				return err
			}
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				rep, err := client.ReplaySpaces(ctx, req)
				if err != nil {
					return err
				}
				printReplay(out, rep)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&policy, "policy", "", "replay under this saved revision number or policy file")
	cmd.Flags().StringVar(&mode, "mode", "", "guard mode to judge in: off, log, block-other-jobs or strict")
	cmd.Flags().BoolVar(&all, "all", false, "list every call, not only those whose verdict changed")
	return cmd
}

// isFixturePath reports whether arg names payload files rather than a log
// owner. Owners are IDs, task:<id> or process:<pid>, which hold no '/' and
// no '.'.
func isFixturePath(arg string) bool {
	return strings.ContainsAny(arg, "/.")
}

// setReplayPolicy puts --policy into req: digits name a revision, anything
// else a policy file.
func setReplayPolicy(req *orchdv1.ReplaySpacesRequest, policy string) error {
	if policy == "" {
		return nil
	}
	if n, err := strconv.ParseInt(policy, 10, 64); err == nil {
		if n <= 0 {
			return fmt.Errorf("--policy: a revision number is 1 or more")
		}
		req.PolicyRevision = n
		return nil
	}
	doc, err := readPolicyFile(policy)
	if err != nil {
		return err
	}
	req.PolicyJson = string(doc)
	return nil
}

func readPolicyFile(path string) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, fmt.Errorf("--policy: %w", err)
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxPolicyFile))
}

// openRegular opens path for reading if it is a regular file. Reason: a
// FIFO or device would block or never end.
func openRegular(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.Open(path)
}

// printReplay prints a replay's counts and its rows.
func printReplay(out io.Writer, rep *orchdv1.ReplaySpacesResponse) {
	pin := "no pin"
	if rep.GetPinned() {
		pin = "pinned to " + cell(rep.GetPinSpace())
	}
	fmt.Fprintf(out, "%s (%s), mode %s: %d calls replayed, %d with a recorded verdict, %d changed, %d with none recorded, %d not decided\n",
		cell(rep.GetOwner()), pin, cell(rep.GetMode()), rep.GetEvents(), rep.GetCompared(), rep.GetChanged(), rep.GetUnrecorded(), rep.GetProblems())
	printReplayRows(out, rep.GetRows())
	if rep.GetTruncated() {
		fmt.Fprintln(out, "the log holds more than one replay reads or lists; the counts and rows above stop there")
	}
}

func printReplayRows(out io.Writer, rows []*orchdv1.ReplayedEvent) {
	if len(rows) == 0 {
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SEQ\tEVENT\tTOOL\tREV\tRECORDED\tREPLAYED\tCHANGED")
	for _, r := range rows {
		recorded := "-"
		if r.GetRecorded() {
			recorded = verdictCell(r.GetRecordedVerdict(), r.GetRecordedRule())
		}
		replayed := verdictCell(r.GetVerdict(), r.GetRule())
		if r.GetProblem() != "" {
			replayed = "not decided: " + r.GetProblem()
		}
		changed := "-"
		if r.GetChanged() {
			changed = "yes"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\t%s\n", r.GetSeq(), cell(r.GetEvent()), cell(r.GetTool()),
			r.GetPolicyRevision(), cell(recorded), cell(replayed), changed)
	}
	_ = w.Flush()
}

// verdictCell is a verdict with its rule id.
func verdictCell(verdict, rule string) string {
	if rule == "" {
		return verdict
	}
	return verdict + " (" + rule + ")"
}

// replayFixtures replays recorded hook payload files under a policy file,
// with no orchd.
func replayFixtures(out io.Writer, path, policy, mode string, all bool) error {
	if policy == "" {
		return fmt.Errorf("replaying payload files needs --policy <policy file>")
	}
	if _, err := strconv.ParseInt(policy, 10, 64); err == nil {
		return fmt.Errorf("replaying payload files needs a policy file; saved revisions are replayed by orchd")
	}
	if mode == "" {
		mode = string(spaces.GuardBlockOtherJobs)
	}
	switch spaces.GuardMode(mode) {
	case spaces.GuardOff, spaces.GuardLog, spaces.GuardBlockOtherJobs, spaces.GuardStrict:
	default:
		return fmt.Errorf("--mode must be off, log, block-other-jobs or strict")
	}
	doc, err := readPolicyFile(policy)
	if err != nil {
		return err
	}
	p, err := spaces.Parse(doc)
	if err != nil {
		return err
	}
	res, err := spaces.Validate(p, spaces.DefaultEnv())
	if err != nil {
		return fmt.Errorf("the policy does not validate: %w", err)
	}
	pol, err := spaces.NewGuardPolicy(res, nil)
	if err != nil {
		return err
	}
	files, err := fixtureFiles(path)
	if err != nil {
		return err
	}
	for _, f := range files {
		sessions, err := readFixtureSessions(f)
		if err != nil {
			return err
		}
		for _, s := range sessions {
			pin := spaces.GuardPin{Found: true, Space: spaces.Classify(spaces.Origin{System: spaces.SystemSession, Path: s.firstCwd}, res, spaces.Options{}).Space}
			rep := spaces.Replay(s.events, pin, spaces.GuardEnforcement{Guard: spaces.GuardMode(mode)},
				func(int64) (spaces.GuardPolicy, error) { return pol, nil })
			resp := &orchdv1.ReplaySpacesResponse{
				Owner: filepath.Base(f) + " " + s.id, Mode: mode, PinSpace: pin.Space, Pinned: true,
				Events: int64(len(rep.Rows)), Compared: int64(rep.Compared), Changed: int64(rep.Changed),
				Unrecorded: int64(rep.Unrecorded), Problems: int64(rep.Problems),
			}
			for _, r := range rep.Rows {
				if all || r.Changed || r.Problem != "" {
					resp.Rows = append(resp.Rows, fixtureRowProto(r))
				}
			}
			printReplay(out, resp)
		}
	}
	return nil
}

func fixtureRowProto(r spaces.ReplayRow) *orchdv1.ReplayedEvent {
	ev := r.Event
	return &orchdv1.ReplayedEvent{
		Seq: ev.Seq, Event: string(ev.Guard.Kind), Tool: ev.Guard.Tool,
		Recorded: ev.Recorded.OK, RecordedVerdict: string(ev.Recorded.Verdict), RecordedRule: string(ev.Recorded.Rule),
		Verdict: string(r.Verdict), Rule: string(r.Rule), Changed: r.Changed, Problem: r.Problem,
	}
}

// fixtureFiles is path itself, or the .jsonl files in it, sorted.
func fixtureFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	switch {
	case info.Mode().IsRegular():
		return []string{path}, nil
	case !info.IsDir():
		return nil, fmt.Errorf("%s is neither a payload file nor a folder", path)
	}
	files, err := filepath.Glob(filepath.Join(path, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s holds no .jsonl payload files", path)
	}
	sort.Strings(files)
	return files, nil
}

// fixtureSession is one session's replayable events from a payload file.
type fixtureSession struct {
	id       string
	firstCwd string
	events   []spaces.ReplayEvent
}

// fixtureLine is one recorded hook payload (internal/hook/capture).
type fixtureLine struct {
	Payload struct {
		Cwd       string          `json:"cwd"`
		Event     string          `json:"hook_event_name"`
		SessionID string          `json:"session_id"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		OldCwd    string          `json:"old_cwd"`
		NewCwd    string          `json:"new_cwd"`
	} `json:"payload"`
}

// maxFixtureLine bounds one payload line.
const maxFixtureLine = 4 << 20

// maxFixtureEvents bounds the events kept from one payload file, as many as
// orchd replays from one log; a variable so tests can reach it.
var maxFixtureEvents = 50000

// readFixtureSessions reads a payload file's tool calls and directory moves,
// by session in the order sessions first appear. Each event's sequence
// number is its line number. Its targets are read from the payload as orchd
// reads a live call's.
func readFixtureSessions(path string) ([]*fixtureSession, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var order []*fixtureSession
	kept := 0
	byID := map[string]*fixtureSession{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxFixtureLine)
	for n := int64(1); sc.Scan(); n++ {
		var l fixtureLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		p := l.Payload
		s := byID[p.SessionID]
		if s == nil {
			s = &fixtureSession{id: p.SessionID, firstCwd: p.Cwd}
			byID[p.SessionID] = s
			order = append(order, s)
		}
		var ev spaces.GuardEvent
		switch p.Event {
		case "PreToolUse":
			ev = spaces.GuardEvent{Kind: spaces.EventPreToolUse, Tool: p.ToolName, Cwd: p.Cwd,
				Targets: spaces.Targets(p.ToolName, p.ToolInput, p.Cwd)}
		case "CwdChanged":
			ev = spaces.GuardEventOf(string(spaces.EventCwdChanged), "", p.Cwd, p.OldCwd, p.NewCwd, "")
		default:
			continue
		}
		if kept++; kept > maxFixtureEvents {
			return nil, fmt.Errorf("%s holds more than %d tool calls and moves; split it", path, maxFixtureEvents)
		}
		s.events = append(s.events, spaces.ReplayEvent{Seq: n, Guard: ev})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return order, nil
}
