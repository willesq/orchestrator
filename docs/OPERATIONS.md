# Running orch

## Status

`orch status` reports on everything: `orchd` and each subsystem (last tick, restarts), the
vector store, on-disk stores and their freshness, harness wiring drift (items you turned off in orch are reported as `N disabled by you`, not
drift), a `<harness> tool hooks` line per harness (whether its tool calls reach orchd for spaces;
Gemini is listed as not covered by spaces), the `safety-net` line
(cc-safety-net installed at the pinned version and the `orch-guards` rules active; anything else
is a warning naming the fix), preconditions, and how much instruction text each harness loads
per session. It works with `orchd` down and always
exits zero; `orch check` is the pass/fail version. Both take `--json`.

## Logs

```sh
orch logs                        # the last 100 lines from every source
orch logs -s qdrant -n 50        # one source
orch logs -f                     # keep printing new lines
```

`orchd` keeps its most recent 5000 log lines in memory, tagged by source: `orchd` itself, each
subsystem, `qdrant` (the engine's own output, in binary mode), `rag` and `database`. The
service log file (`brew services` writes `$(brew --prefix)/var/log/orch/orchd.log`) keeps
everything `orchd` logs; the engine's output goes only to memory.

## Restart

```sh
orch restart                     # bounce orchd through its service manager
orch restart --list              # what can be restarted
orch restart sources-scheduler   # one subsystem, in place
orch restart subsystems          # every subsystem, in place
```

An in-place restart keeps the socket, the loaded embedder and the other subsystems running.
Subsystems: `qdrant` (binary mode), `workflow-worker`, `sources-scheduler`, `docs-scheduler`,
`pr-babysitter`, `routing-evaluator`, `handoff`, `session-summarizer`, `settings-watcher`,
`self-update`.

## Claude settings

`orch settings list` shows the Claude Code hooks, permission rules and plugins and the
cc-safety-net rules orch manages; `orch settings enable|disable <kind> <id>` turns one on or off,
as the app's Settings → Claude section does. The `settings-watcher` subsystem reads the files every 2 s and only
imports what changed outside orch; it never writes. A change is written at once, after a one-time
backup to `~/.local/share/orch/backups`, and refused if the file changed underneath (try again).
A file that does not parse shows as an error and blocks changes until it is fixed. State is per
machine. See [internals/settings.md](internals/settings.md).

## Spaces

```sh
orch spaces draft < policy.json       # save a draft revision; changes nothing
orch spaces diff [--full]             # what the newest draft changes, and its problems
orch spaces apply --revision N        # enforce draft N
orch spaces apply --revision N --confirm-github-org ORG   # also accept ORG's changed GitHub identity
orch spaces why <path|url|owner/repo|slack:ID|teams:ID>   # which space its content goes to, and why
orch spaces log [<session>|task:ID]   # a session's command log, in order
orch spaces log summary [--since 168h]   # per space: tool calls, directory moves, moves into another space
orch spaces log verify <session|task:ID>   # check a session's command log was not changed
orch spaces enforce                   # every enforcement layer's switch
orch spaces enforce guard off --for 15m   # switch a layer, optionally for a time (operator only)
orch sessions list [--all]   # sessions and their state: active, idle, ended (with --all), unpinned
orch run <space> -- claude   # start Claude Code with the space's profile, from any folder
orch run <space> -- codex    # the same for Codex
```

The spaces policy lives in `orch.db` as numbered revisions. Diff and apply are yours alone: orchd
refuses them inside Claude Code or Codex sessions and inside queued tasks, so run them in your own
terminal (or use Orch.app). Apply takes only the newest draft, the one you reviewed, and only when
it validates. An agent session may save a draft. The diff says when it did and shows every value
it changes, and applying it needs `--accept-agent-draft`. A saved draft, or an edit to `orch.db`, changes nothing that is enforced; `orch status`
shows it in its `spaces` section as drift.

Apply looks up every bound GitHub org with `gh` and records its node ID, so it needs GitHub to be
reachable. If an org's login is later deleted and registered by someone else, the next docs sync
sees a different node ID. orchd then stops syncing that org, `orch status` reports an unconfirmed
org, and `orch spaces diff` names it. Check the org on GitHub. If the change is expected, apply the
newest revision again with `--confirm-github-org ORG`. Until you do, any apply is refused. See
[internals/config.md](internals/config.md#spaces-policy).

Each isolated space syncs its GitHub orgs only as its own account: set the space's
`gh_config_dir` and run `GH_CONFIG_DIR=<dir> gh auth login` ([INSTALL.md](INSTALL.md#github-accounts-per-space)).
`orch status` names any space whose profile is missing or not logged in, and warns when one
profile's account can see another job's org.

`orch spaces why` shows how the applied policy places a target: every bound root holding it from
shallowest to deepest, what its GitHub remote contributes, the rule that decided and the result.
For a path it also says where a session started there would write and which space it would read
as. Inside an agent session orchd answers only when every space in the answer is public. See
[internals/memory.md](internals/memory.md#placement).

orchd keeps a command log of every tool call and directory change Claude Code and Codex sessions
report, in `~/.local/share/orch/toollog.db` (owner-only, never indexed into RAG). Commands are
stored credential-redacted and capped at 2 KB. `orch spaces log <session>` prints a session's
records; inside an agent session it prints only that session's own. `orch spaces log summary`
sums the last week per space, including moves into a directory of another space; it is
operator-only. `orch spaces log verify <session>` checks that no
record was changed, removed or moved since orchd wrote it; it is operator-only. A session's
records are deleted 7 days after it ends and its process has exited (Settings, "Command log":
3, 7 or 30 days); refused calls are kept 90 days. A running session is never deleted, however
long it sits idle, only cut to its newest 50 MB, and past 500 MB in total the sessions that
ended first go early, all but their refused calls. `verify` shows how far retention cut a
session, and orchd's log names every cut. See [internals/daemon.md](internals/daemon.md#command-log).

Each enforcement layer has a switch for isolating behavior when debugging: `memory` (memory
filtering), `guard` (`block-other-jobs` by default, `strict`, `log` or `off`), `terms`
(outbound terms), `sandbox` (generated sandbox rules) and `hooks` (Claude Code's pre-tool-use and
cwd-changed hooks). Only you can switch them, from your own terminal or Orch.app; `--for` makes
the switch revert by itself. Every change is logged with its time and caller. While a layer is
off, `orch status` and every new session's start digest say so, and the command log keeps
recording. orch settings and `orch install` cannot turn the tool hooks off; `orch spaces enforce
hooks off` can. See [internals/daemon.md](internals/daemon.md#enforcement-switches).
In Orch.app, Settings → Spaces (and the menu bar panel) has the same switches with their timers,
shows a banner while any layer is off, and checks a folder: every root holding it, what its git
remote changes, and the space it lands in, as `orch spaces why` reports. See
[internals/app.md](internals/app.md#spaces).

**Per-space harness settings.** While a policy is applied and the `sandbox` layer is on, orchd
keeps `.claude/settings.local.json` in every git checkout under a space's roots: the OS sandbox on,
and other jobs' folders denied to shell commands and to Claude Code's file tools, plus the space's
`GH_CONFIG_DIR` and its own `TMPDIR` (Claude Code's sandboxed commands get the space's `TMPDIR`
only in sessions started with `orch run`; sessions started in a checkout share Claude Code's
`/tmp/claude-<uid>`). Your own entries in that file stay; orch adds the file to the
checkout's `.git/info/exclude` when nothing ignores it, and never writes a tracked one. A new clone
gets its file within a minute. `orch spaces enforce sandbox off` takes orch's entries out again.

`orch run <space> -- claude|codex` gives a session the same rules from any folder (Codex has no
per-checkout settings, so this is how Codex sessions get them). Claude Code then runs with a
per-space `CLAUDE_CONFIG_DIR`, which starts logged out: the first time, run
`orch run <space> -- claude` and `/login` in it. Log in to the space's gh profile with
`GH_CONFIG_DIR=<gh_config_dir> gh auth login`. Run it in a folder of that space or in one no space
holds; `--force` allows another space's folder. See
[internals/install.md](internals/install.md#per-space-harness-settings).

`orch sessions list` shows each pinned session's space, directory and state. orchd reads it from
the kernel: a session left open without `/exit` stays `idle` for as long as its claude or codex
process runs, and is marked `ended` within a minute of the process exiting, even without
SessionEnd. `claude --resume` in a new process keeps the session's pin and space. Running claude,
codex or gemini processes with no pin (started before the hooks were installed) show as
`unpinned`. See [internals/daemon.md](internals/daemon.md#session-liveness).

## Upgrades

After an upgrade, the `self-update` subsystem notices the new `orchd` binary within a minute
and exits; `brew services` (or the launchd agent install writes) starts the new one. Running
tasks are durable and resume. If `orch status` shows the `build` line as `drifted`, `orchd` was
started by hand with nothing to restart it: run `orch restart`.

An orch release that pins a newer cc-safety-net shows `safety-net outdated` until
`orch install --apply` (or Upgrade in Orch.app) moves the plugin to the new pin.
