package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/github"
)

// spaceLogin is which gh login and which Claude Code configuration a space
// uses, and whether each is logged in (SP-074.3, D-59). It is read from
// 'gh auth status' and 'claude auth status' metadata only: no token, cookie
// or credential file is read, logged or stored.
type spaceLogin struct {
	Space  string
	Kind   string
	GH     loginState
	Claude loginState
}

// loginState is one login a space uses.
type loginState struct {
	// Own is true when the space names its own folder (gh_config_dir or
	// claude_config_dir); false is the default login or configuration.
	Own bool
	Dir string // the space's own folder; "" for the default
	// State is loginLoggedIn, loginLoggedOut, loginRefused or
	// loginUnknown.
	State string
	// Account is the gh login, or Claude Code's sign-in method (such as
	// "claude.ai"); never an email or organization.
	Account string
	// Storage is where gh keeps the token: "keyring", "file" or the
	// variable it came from. Always "" for Claude Code.
	Storage string
	Detail  string // why, for a state other than logged in
}

// The states of a login.
const (
	loginLoggedIn  = "logged in"
	loginLoggedOut = "not logged in"
	loginRefused   = "refused" // a gh profile directory orch will not use
	loginUnknown   = "unknown" // the check itself could not run
)

// claudeLogins checks Claude Code configurations with 'claude auth status',
// caching each answer like the gh profile checks.
type claudeLogins struct {
	// run runs 'claude auth status --json' with env; runClaudeAuth outside
	// tests.
	run func(ctx context.Context, env []string) ([]byte, error)
	now func() time.Time

	mu      sync.Mutex
	checked map[string]claudeCheck
}

type claudeCheck struct {
	state, method, detail string
	at                    time.Time
}

func newClaudeLogins() *claudeLogins {
	return &claudeLogins{run: runClaudeAuth, now: time.Now, checked: map[string]claudeCheck{}}
}

// runClaudeAuth runs the user's Claude Code CLI as 'claude auth status
// --json' with env.
func runClaudeAuth(ctx context.Context, env []string) ([]byte, error) {
	bin := agentrun.FindClaude("", userHome())
	if bin == "" {
		return nil, errors.New("the claude command was not found")
	}
	cmd := exec.CommandContext(ctx, bin, "auth", "status", "--json")
	cmd.Env = env
	return cmd.Output()
}

// claudeOverrides are variables that would make 'claude auth status' report
// a credential from the environment instead of the configuration's own
// login, as GH_TOKEN would for gh.
var claudeOverrides = []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

// claudeEnv is the environment for checking Claude Code configuration dir:
// this process's, without claudeOverrides, and with CLAUDE_CONFIG_DIR only
// when dir is set.
func claudeEnv(dir string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(claudeOverrides, name)
	})
	if dir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	}
	return env
}

// check reports configuration dir's login ("" is the user's own).
func (c *claudeLogins) check(ctx context.Context, dir string) claudeCheck {
	c.mu.Lock()
	got, ok := c.checked[dir]
	c.mu.Unlock()
	if ok {
		ttl := profileCheckTTL
		if got.state != loginLoggedIn {
			ttl = profileRetryTTL
		}
		if c.now().Sub(got.at) < ttl {
			return got
		}
	}
	// Reason: as for gh profiles, a caller giving up early must not cache a
	// failure.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), profileCheckTimeout)
	defer cancel()
	got = claudeCheck{at: c.now()}
	out, err := c.run(cctx, claudeEnv(dir))
	// Reason: only these two fields are decoded, so the email and
	// organization 'claude auth status' prints are never kept.
	var st struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	switch {
	case err != nil && len(out) == 0:
		got.state, got.detail = loginUnknown, github.Printable(err.Error(), 256)
	case json.Unmarshal(out, &st) != nil:
		got.state, got.detail = loginUnknown, "claude auth status gave no readable answer"
	case st.LoggedIn:
		got.state, got.method = loginLoggedIn, github.Printable(st.AuthMethod, 32)
	default:
		got.state, got.detail = loginLoggedOut, "log in with: orch run <space> -- claude, then /login"
	}
	if ctx.Err() != nil && got.state == loginUnknown {
		return got
	}
	c.mu.Lock()
	c.checked[dir] = got
	c.mu.Unlock()
	return got
}

// spaceLogins reports, for every space of the applied policy, which gh login
// and Claude Code configuration it uses and their state. It names spaces and
// folders, so only the operator may see it.
func (s *server) spaceLogins(ctx context.Context) ([]spaceLogin, error) {
	if s.spaces == nil {
		return nil, nil
	}
	res, err := s.spaces.Current()
	if err != nil || !res.Configured() {
		return nil, err
	}
	var out []spaceLogin
	for _, name := range res.Names() {
		sp, _ := res.Space(name)
		l := spaceLogin{Space: name, Kind: string(sp.Kind)}
		l.GH = s.ghLogin(ctx, res.GHConfigDir(name))
		l.Claude = s.claudeLogin(ctx, res.ClaudeConfigDir(name))
		out = append(out, l)
	}
	return out, nil
}

// ghLogin reports gh profile dir ("" is the default login).
func (s *server) ghLogin(ctx context.Context, dir string) loginState {
	st := loginState{Own: dir != "", Dir: dir}
	g := s.ghProfiles
	if g == nil {
		st.State, st.Detail = loginUnknown, "orchd runs without GitHub"
		return st
	}
	if dir != "" && g.checkDir != nil {
		if err := g.checkDir(dir); err != nil {
			st.State, st.Detail = loginRefused, github.Printable(err.Error(), 1024)
			return st
		}
	}
	key := dir
	if dir == "" {
		key = defaultProfile
	}
	c := g.check(ctx, key)
	switch {
	case c.err != nil && c.login != "":
		// Logged in, but its orgs could not be listed (offline, say).
		st.State, st.Account, st.Storage = loginUnknown, c.login, c.storage
		st.Detail = "logged in, but GitHub could not be asked for its orgs; checked again within a minute"
		return st
	case c.err != nil:
		// Reason: a fixed hint rather than gh's own error text.
		st.State, st.Detail = loginLoggedOut, "run: gh auth login"
		if dir != "" {
			st.Detail = fmt.Sprintf("run: GH_CONFIG_DIR=%s gh auth login", shellQuote(github.Printable(dir, 1024)))
		}
		return st
	}
	st.State, st.Account, st.Storage = loginLoggedIn, c.login, c.storage
	return st
}

// claudeLogin reports Claude Code configuration dir ("" is the user's own).
func (s *server) claudeLogin(ctx context.Context, dir string) loginState {
	st := loginState{Own: dir != "", Dir: dir}
	if s.claudeLogins == nil {
		st.State, st.Detail = loginUnknown, "not checked"
		return st
	}
	c := s.claudeLogins.check(ctx, dir)
	st.State, st.Account, st.Detail = c.state, c.method, c.detail
	return st
}
