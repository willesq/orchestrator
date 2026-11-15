package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/procstat"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

const (
	// maxUnlockFor is the longest unlock orchd grants (SP-077).
	maxUnlockFor = 60 * time.Minute
	// unlockWait bounds the wait for Orch.app; orchd lets a request lapse
	// after two minutes.
	unlockWait = 3 * time.Minute
)

// runUnlocked runs argv with the terminal's input and output and returns
// its exit status; tests replace it.
var runUnlocked = func(argv []string, in io.Reader, out, errOut io.Writer) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errOut
	// Reason: Ctrl-C and Ctrl-\ reach the whole foreground group; the
	// shell handles them, and orch must outlive it to end the unlock.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}

// unlockTTY is where the code Orch.app shows is typed; tests replace it.
var unlockTTY = "/dev/tty"

// readUnlockCode reads one line from the terminal itself, not from
// standard input, so a pipe cannot answer for the user.
func readUnlockCode() (string, error) {
	f, err := os.Open(unlockTTY)
	if err != nil {
		return "", fmt.Errorf("the code must be typed at a terminal: %w", err)
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading the code: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// obtainUnlock asks orchd for an unlock, waits for Orch.app to confirm it,
// then sends the code Orch.app shows, as the user types it here. Reason:
// the code goes from the app, which only the user sees, to this terminal,
// so the user cannot be tricked into confirming a request an agent filed
// at the same time.
func obtainUnlock(client orchdv1.OrchdClient, forDur time.Duration, errOut io.Writer) (*orchdv1.OperatorUnlock, error) {
	where := "no terminal"
	if t, err := procstat.TTY(os.Getpid()); err == nil && t != "" {
		where = "terminal " + t
	}
	fmt.Fprintf(errOut, "Asking Orch.app to unlock operator scope for %s (process %d, %s).\n"+
		"Confirm it in Orch.app's menu bar panel only if it shows the same process and terminal…\n",
		forDur, os.Getpid(), where)
	// Reason: Ctrl-C while waiting withdraws the request.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	ctx, cancel := context.WithTimeout(ctx, unlockWait)
	rep, err := client.RequestUnlock(ctx, &orchdv1.RequestUnlockRequest{ForSeconds: int64(forDur / time.Second)})
	cancel()
	stop()
	if err != nil {
		return nil, err
	}
	id := rep.GetUnlock().GetId()
	for {
		fmt.Fprint(errOut, "Confirmed. Type the code Orch.app shows now: ")
		code, err := readUnlockCode()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		got, err := client.ProveUnlock(ctx, &orchdv1.ProveUnlockRequest{Id: id, Code: code})
		cancel()
		if grpcstatus.Code(err) == codes.InvalidArgument {
			fmt.Fprintf(errOut, "%s\n", grpcstatus.Convert(err).Message())
			continue
		}
		if err != nil {
			return nil, err
		}
		return got.GetUnlock(), nil
	}
}

// unlockShell is the shell an unlock starts when no command is given.
func unlockShell() []string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return []string{sh}
	}
	return []string{"/bin/sh"}
}

func newSpacesUnlockCmd(cf clientFactory) *cobra.Command {
	var forDur time.Duration
	cmd := &cobra.Command{
		Use:   "unlock [--for 15m] [-- <command> [args...]]",
		Short: "Ask Orch.app for operator scope in this terminal for a while",
		Long: `While a spaces policy is applied, only Orch.app is the operator (D-71): a
terminal reads public memory only and operator actions such as
'orch rag relabel' or 'orch spaces apply' are refused there.

'orch spaces unlock' asks Orch.app to confirm operator scope for this
process and what it starts: it reads every space and runs operator commands
such as 'orch rag relabel', 'orch spaces retag' and 'orch spaces diff'.
Applying a policy, switching enforcement off or loosening it, the sandbox
setting, outbound rules and the settings Orch.app keeps for itself stay in
Orch.app even then (D-78). Orch.app shows the request with this command,
its process and the terminal it runs in. Confirm it there (it asks for Touch
ID or your password); Orch.app then shows a code, which you type here.
Nothing is granted until the code matches, so a request an agent filed at
the same time gets nothing even if it is confirmed by mistake. Once confirmed, orch starts your shell ($SHELL), or the
command after --, as the operator until the duration ends (at most 60
minutes), the shell exits, or 'orch spaces lock' ends it. An agent started in
that shell is never the operator, and its first call to orchd ends the
unlock. Prefer the one-command form: an unlocked shell is as trusted as
anything that can type into it.

  orch spaces unlock --for 15m                       an operator shell
  orch spaces unlock --for 30m -- orch rag relabel   one command

Refused inside an agent session (Claude Code, Codex) and inside tasks orchd
runs. With no policy applied a terminal is the operator already.`,
		Args: func(c *cobra.Command, args []string) error {
			if len(args) > 0 && c.ArgsLenAtDash() != 0 {
				return fmt.Errorf("put the command after --, e.g. 'orch spaces unlock --for 15m -- orch rag relabel'")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			if forDur < time.Second || forDur > maxUnlockFor {
				return fmt.Errorf("--for must be from 1s to %s, e.g. 15m", maxUnlockFor)
			}
			argv := args
			if len(argv) == 0 {
				argv = unlockShell()
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			out, errOut := c.OutOrStdout(), c.ErrOrStderr()
			u, err := obtainUnlock(client, forDur, errOut)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Unlocked until %s: %s and what it starts are the operator. Exit it or run 'orch spaces lock' to end it sooner.\n",
				time.Unix(u.GetExpiresAt(), 0).Local().Format("15:04:05"), argv[0])
			code, runErr := runUnlocked(argv, c.InOrStdin(), out, errOut)
			// Reason: the unlock ends with the process it was for, not only
			// with its duration.
			lockCtx, lockCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer lockCancel()
			if _, err := client.Lock(lockCtx, &orchdv1.LockRequest{Id: u.GetId()}); err != nil {
				fmt.Fprintf(errOut, "warning: the unlock could not be ended (it ends by itself at %s): %v\n",
					time.Unix(u.GetExpiresAt(), 0).Local().Format("15:04:05"), err)
			} else {
				fmt.Fprintln(errOut, "Locked.")
			}
			switch {
			case runErr != nil:
				return runErr
			case code != 0:
				return fmt.Errorf("%s exited with status %d", argv[0], code)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&forDur, "for", 15*time.Minute, "how long the unlock lasts (at most 60m)")
	return cmd
}

func newSpacesLockCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "End the operator unlocks that cover this terminal",
		Long: `Ends the unlocks ('orch spaces unlock') that cover this process: those
granted to it or to one of its parents. Orch.app can end any unlock.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return withClient(cf, func(ctx context.Context, client orchdv1.OrchdClient) error {
				rep, err := client.Lock(ctx, &orchdv1.LockRequest{})
				if err != nil {
					return err
				}
				if rep.GetEnded() == 0 {
					fmt.Fprintln(c.OutOrStdout(), "No unlock covers this terminal.")
					return nil
				}
				fmt.Fprintf(c.OutOrStdout(), "Ended %d unlock(s).\n", rep.GetEnded())
				return nil
			})
		},
	}
}
