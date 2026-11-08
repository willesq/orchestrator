# Running orch

## Status

`orch status` reports on everything: `orchd` and each subsystem (last tick, restarts), the
vector store, on-disk stores and their freshness, harness wiring drift (items you turned off in orch are reported as `N disabled by you`, not
drift), the `safety-net` line
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

`orch spaces why` shows how the applied policy places a target: every bound root holding it from
shallowest to deepest, what its GitHub remote contributes, the rule that decided and the result.
For a path it also says where a session started there would write and which space it would read
as. Inside an agent session orchd answers only when every space in the answer is public. See
[internals/memory.md](internals/memory.md#placement).

## Upgrades

After an upgrade, the `self-update` subsystem notices the new `orchd` binary within a minute
and exits; `brew services` (or the launchd agent install writes) starts the new one. Running
tasks are durable and resume. If `orch status` shows the `build` line as `drifted`, `orchd` was
started by hand with nothing to restart it: run `orch restart`.

An orch release that pins a newer cc-safety-net shows `safety-net outdated` until
`orch install --apply` (or Upgrade in Orch.app) moves the plugin to the new pin.
