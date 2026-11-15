package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// AzRest is the Graph transport: it runs the Azure CLI's `az rest`, logged
// in to the profile directory ConfigDir, for each request. The user logs
// that profile in themselves (AZURE_CONFIG_DIR=<dir> az login); the Graph
// token stays inside az and its profile. orch never reads files in the
// profile, never asks az for a token, and keeps none of az's error output,
// which is not orch's to vouch for (TestNoCodeReadsAzureCredentials).
type AzRest struct {
	// Bin is the az executable; "" finds az on PATH.
	Bin string
	// ConfigDir is the profile directory, given to az as AZURE_CONFIG_DIR.
	ConfigDir string
}

// ErrNotLoggedIn is returned when the profile has no working az login.
var ErrNotLoggedIn = errors.New("the Teams profile is not logged in; run `AZURE_CONFIG_DIR=<dir> az login` yourself")

// ErrAz marks a failed az run.
var ErrAz = errors.New("az failed")

// profileOverrides are environment variables that would make az use
// another profile than ConfigDir.
var profileOverrides = []string{"AZURE_CONFIG_DIR"}

// env is the environment az runs with: the caller's, with ConfigDir as the
// profile.
func (a AzRest) env() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(profileOverrides, name)
	})
	return append(env, "AZURE_CONFIG_DIR="+a.ConfigDir)
}

// run runs az with args and returns its standard output.
func (a AzRest) run(ctx context.Context, args ...string) ([]byte, error) {
	if a.ConfigDir == "" {
		return nil, fmt.Errorf("%w: no profile directory", ErrAz)
	}
	bin := a.Bin
	if bin == "" {
		bin = "az"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = a.env()
	stdout := &capped{max: maxOutput}
	stderr := &capped{max: 64 << 10, drop: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if stdout.over {
			return nil, fmt.Errorf("%w: %s %s answered more than %d MiB", ErrAz, bin, args[0], maxOutput>>20)
		}
		// Reason: az's messages are not shown or kept, since orch cannot
		// tell what they hold; only whether the login is missing is read.
		if strings.Contains(stderr.buf.String(), "az login") {
			return nil, ErrNotLoggedIn
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%w: %s %s exited with status %d", ErrAz, bin, args[0], exit.ExitCode())
		}
		return nil, fmt.Errorf("%w: %s cannot be run", ErrAz, bin)
	}
	return stdout.buf.Bytes(), nil
}

// maxOutput bounds one az answer: a Graph page is at most 50 messages.
var maxOutput = 64 << 20

// capped keeps at most max bytes written to it. Past that it fails the
// write, so az is stopped rather than held in memory whole, or with drop
// set it discards the rest.
type capped struct {
	buf  bytes.Buffer
	max  int
	drop bool
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.over = true
		if c.drop {
			return len(p), nil
		}
		return 0, errors.New("output too large")
	}
	return c.buf.Write(p)
}

// Get runs `az rest --method get --url <rawURL>`; az attaches the profile's
// Graph login. It refuses a URL outside GraphBase.
func (a AzRest) Get(ctx context.Context, rawURL string) ([]byte, error) {
	if !strings.HasPrefix(rawURL, GraphBase+"/") {
		return nil, fmt.Errorf("%w: %s is not a Microsoft Graph URL", ErrAz, rawURL)
	}
	return a.run(ctx, "rest", "--method", "get", "--url", rawURL, "--output", "json")
}

// Account is a profile's login as `az account show` reports it.
type Account struct {
	// Tenant is the tenant ID the profile is logged in to.
	Tenant string
	// User is the signed-in account's name.
	User string
}

// Status reports the profile's login from `az account show` metadata; it
// never asks for a token.
func (a AzRest) Status(ctx context.Context) (Account, error) {
	out, err := a.run(ctx, "account", "show", "--output", "json")
	if err != nil {
		return Account{}, err
	}
	var v struct {
		TenantID string `json:"tenantId"`
		User     struct {
			Name string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.TenantID == "" {
		return Account{}, fmt.Errorf("%w: az account show gave no tenant", ErrAz)
	}
	return Account{Tenant: v.TenantID, User: v.User.Name}, nil
}
