# Running orch

## Status

`orch status` reports on everything: `orchd` and each subsystem (last tick, restarts), the
vector store, on-disk stores and their freshness, harness wiring drift (items you turned off in orch are reported as `N disabled by you`, not
drift), while spaces are on a `<harness> tool hooks` line per harness (whether its tool calls
reach orchd; Gemini is listed as not covered by spaces) and a `spaces` section once a policy
revision exists, the `safety-net` line
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
`pr-reviews`, `pr-babysitter`, `routing-evaluator`, `handoff`, `session-summarizer`, `settings-watcher`,
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

Memory spaces keep work for different jobs apart. They are **off by default**; while they are
off, orch works as it did before spaces. [SPACES.md](SPACES.md) is the guide: turning them on and
off, writing and applying a policy, who the operator is (Orch.app once a policy is applied, or a
terminal you unlock from it), the enforcement layers and their switches, the command log,
outbound terms, per-space harness settings, harness coverage and the limits. Day to day:

```sh
orch spaces on | off --confirm        # the master switch (Orch.app: Settings → Spaces)
orch spaces enforce                   # every enforcement layer's state
orch spaces why <path|url|owner/repo> # where a target's content goes, and why
orch spaces log summary               # tool calls and moves per space, last week
orch sessions list                    # sessions, their space and state
orch spaces unlock --for 15m          # operator scope for this terminal, confirmed in Orch.app
```

## Uninstall

`orch uninstall` previews; `--apply` removes everything orch install and orchd put in place and
keeps your data, `--purge-data` deletes that too. See
[INSTALL.md](INSTALL.md#uninstalling) for what it removes and what it leaves for you.

## Upgrades

After an upgrade, the `self-update` subsystem notices the new `orchd` binary within a minute
and exits; `brew services` (or the launchd agent install writes) starts the new one. Running
tasks are durable and resume. If `orch status` shows the `build` line as `drifted`, `orchd` was
started by hand with nothing to restart it: run `orch restart`.

An orch release that pins a newer cc-safety-net shows `safety-net outdated` until
`orch install --apply` (or Upgrade in Orch.app) moves the plugin to the new pin.
