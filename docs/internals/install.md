# Install and harness adapters

`orch install [--apply|--yes] [--target auto|all|claude-code|codex|gemini] [--home DIR] [--bin-dir DIR]
[--no-model] [--no-daemon] [--no-git-hooks] [--no-app]` sets orch up, and `orch uninstall` takes it
out again ([below](#uninstall)). Both preview by default; `--home` relocates everything for testing.

## Units: install and uninstall are paired

Everything orch puts on a machine is a **unit** (`internal/install/unit.go`): one concern, with
its install and its uninstall defined together, in the same place.

```go
Unit{Name, Shared, Edits []Edit, Steps, UndoSteps func() []Step, Dirs, InstalledBy, Manual, Leftovers}
Edit{Path, Compute(old) → new, Reverse(cur) → Undo{Data | Remove}, Normalize, Unlink}
```

**The pairing rule.** Every install action has its reverse beside it: each `Edit` has `Compute`
and `Reverse`, and a unit with `Steps` has `UndoSteps`. A unit that install does not put in place
itself (the data directory, per-checkout settings orchd writes) names what does in `InstalledBy`
and still has an uninstall. `ValidateUnits` enforces this; a plan built from a unit that breaks it
fails before printing or writing anything, and `TestEveryRegisteredUnitIsPaired` checks every
registered unit. To add an install step, add it to a unit together with its reverse, and extend
the round-trip test (`TestInstallUninstallRoundTrip`) if it touches a new file.

**One registered list.** `install.FullUnits` is the list, in install order: prerequisites (gh
login, which only the user can do), data, checkout settings, the embedding model, then each
harness adapter's units (`Adapter.Units`), then git hooks, the daemon and the app. `InstallPlan`
walks it in order (every edit, then every step); `UninstallPlan` walks it in reverse (every undo
step, then every edit's `Reverse`, then each unit's `Dirs` left empty, deepest first). Units shared
by several harnesses (the shared policy, the modules, the alias, the launchd agent) and repeated
edits of one path appear once. `Manual` and `Leftovers` are printed at the end of a plan as what
the user must do by hand.

- **Reverse** gets the file as it is now and returns it with orch's part taken out, or `Remove`
  when nothing else is in it. Structured files are reversed by meaning: hook entries whose every
  command runs `orch-hook` (the configured binary or one named `orch-hook`) or a script under the
  retired `~/.claude/orchestrator/hooks/` (stricter than install, which replaces any entry
  mentioning `orch-hook`, since uninstall deletes), MCP servers whose command is
  `<dir>/orch mcp <server>`, orch's allow rules, an empty `attribution.commit`, the `enabledPlugins` entry of orch's plugin, and then
  any event, list or object left empty; a file left as `{}` is removed (except `~/.claude.json`).
  The credential-file Read deny rules are kept and named: a deny only narrows, and install did not
  record whether the user had the same rule before.
  Text files lose orch's marked block (and the blank line install put before it). Whole files orch
  owns are removed; an agent or skill file only when it carries `managed-by: orch`, the plist only
  when it names `com.orchestrator.orchd`. Unparseable files and broken markers refuse, as install
  does. Toggles are not read: uninstall removes orch's items whether on or off, so it needs no
  `orch.db`.

## Edit model

Each adapter returns its units; their edits are `{Path, Compute(old) → new, summary, Reverse,
Normalize, Unlink}`.

- **Plan:** every edit is computed before anything is written; one error writes nothing. An edit
  counts as a change only if the bytes differ and `Normalize(old) != new`, so drift is judged by
  meaning (reordered JSON is not drift).
- **Preview:** `DRY RUN`, then `=== CREATE|MODIFY|UNCHANGED <path> ===` with summaries.
- **Apply:** re-read each file and abort if it changed since planning; back up non-empty originals
  to `<data>/backups/<UTC timestamp>/<parent>__<name>` (directory 0700, file 0600, created
  `O_EXCL|O_NOFOLLOW`, because harness config can hold OAuth state); remove planned symlinks; write
  0644. JSON is 2-space indented; TOML via go-toml v2.
- Re-running is idempotent. `orch status` reports any pending edit as drift.

## Claude Code

| File | Edit |
|---|---|
| `~/.claude/settings.json` | hooks, MCP servers, permissions, credential-file deny rules, `attribution.commit` |
| `~/.claude.json` | MCP servers, only if the file exists |
| `~/.claude/CLAUDE.md` | managed block importing the shared policy |
| `~/.claude/agents/*.md`, `skills/*/SKILL.md` | shipped agents and skills |
| `~/.zprofile`, LaunchAgent | see [common](#common) |
| `<data>/marketplaces/orch-pinned/.claude-plugin/marketplace.json`, `~/.cc-safety-net/rules/` | the safety net, below |

**Hooks:** for each event, `hooks.<Event>` gets
`{"hooks": [{"type": "command", "command": "<bin>/orch-hook <slug>"}]}`. Events: `SessionStart`,
`UserPromptSubmit`, `Stop`, `SessionEnd`, `PreCompact`, `PreToolUse`, `PostToolUse`,
`SubagentStop`, `CwdChanged`, mapped to the slugs in [hooks.md](hooks.md). The entries carry no
`matcher`, so `PreToolUse` runs before every tool call. An entry is ours if its command contains the bin path,
`orch-hook`, or `orchestrator/hooks/`; ours is updated in place, duplicates removed, and other
tools' entries kept in order.

Each command also carries a `timeout` in seconds, so a stalled orchd cannot hold a turn for
Claude Code's 600 s default (30 s on `UserPromptSubmit`): `SessionStart` and `UserPromptSubmit`
10, `Stop`, `PreToolUse`, `PostToolUse`, `SubagentStop` and `CwdChanged` 5, `SessionEnd` and
`PreCompact` 15. SessionEnd
hooks otherwise share a 1.5 s budget, which Claude Code raises to the largest per-hook SessionEnd
`timeout` (up to 60 s), so the explicit 15 s keeps session capture from being cut off.
`PostToolUse` and `CwdChanged` are also `"async": true`: they only hand work to orchd and print
nothing, so Claude does not wait for them (Claude Code does not enforce `timeout` on async hooks; it is kept as a
bound if async is removed). In `claude -p`, an async hook still running at teardown is killed;
the hand-off write takes milliseconds, so this is not expected to lose work. An install from
before these fields reads as drift, and `--apply` adds them in place. `PreToolUse` stays
synchronous: only a hook Claude waits for can deny a call, which the spaces tool guard needs. It
is fast because it makes one orchd call with a 20 ms deadline ([hooks.md](hooks.md#tool-hooks)).

**MCP servers:** `mcpServers.rag = {"command": "<orch>", "args": ["mcp", "rag"]}`, likewise `queue`.

**Permissions:** for each server, append `mcp__<server>__*` to `permissions.allow` so hooks and
background agents, which cannot answer prompts, can use memory. Skip it if a `deny` or `ask` rule
covers the server (`mcp__*`, `mcp__<s>`, or any `mcp__<s>__…` rule), and report why; skip if an
allow rule already covers it. Also append the single Bash rule
`Bash(sh ~/.claude/agents/exploit-finder-sandbox.sh:*)`, which lets `exploit-finder` run its PoC
sandbox wrapper without a per-run prompt a nested agent can't answer; it allows only that one
command (the wrapper, not free-form `docker`), is skipped when the user has a matching `deny` or
`ask` rule, and is re-added on the next install if deleted (an uninstall does not remove it, so a
`deny` rule is the opt-out). A `permissions` value of an unexpected JSON type aborts the install.

**Credential files and attribution:** `permissions.deny` gains `Read(~/.config/gh/**)`,
`Read(~/.netrc)` and `Read(~/.docker/config.json)`, each only if absent, so the file tools cannot
read them (cc-safety-net already blocks shell access). `attribution.commit` is set to `""`, so
Claude Code adds no co-author trailer, per the shared policy; other `attribution` keys are kept.
A `permissions.deny` or `attribution` of an unexpected JSON type aborts the install. No `ask`
rule is ever written: guards deny narrowly and never prompt.

**Agents and skills:** everything under `internal/install/assets/claude/` (embedded with `all:`,
so `agents/_shared` is included), grouped by `install.ClaudeAssets` into units a machine turns on
or off:

- **Agents:** `adversarial-qa`, `adversarial-reviewer`, `dev`, `dispatcher`,
  `documentation-manager`, `exploit-finder`, `feature-coder`, `initializer`, `ops`, `pr-verdict`, `research`,
  `reviewer`, `security-audit`, `state-recovery`, `triage`, `validation`. An agent owns
  `agents/<name>.md`, any `agents/<name>-*` helper (`exploit-finder-sandbox.sh`, the PoC sandbox
  wrapper) and any `agents/_shared/<name>-*/` directory (`adversarial-qa-references/`).
- **Skills:** `adversarial-qa`, `api-design`, `qa-fixer`, `orch-dispatch`, `orch-pr-status`,
  `orch-rag-ingest`, `orch-rag-query`, `orch-repo-onboard`, `orch-task-status`. A skill owns
  everything under `skills/<name>/`.
- **Adding one:** add its files there and rebuild. A file no agent or skill owns, or one without
  the marker, fails the build's tests.

Each file carries a `managed-by: orch` marker:
- **Created or updated:** missing files are created and marked files updated.
- **Legacy links:** symlinks into the retired `~/.claude/orchestrator` checkout are replaced.
- **Adopted:** an unmarked file is taken over when it is byte for byte the shipped copy without
  its marker line (the user's own copy that orch now ships).
- **Refused:** any other file aborts the install rather than being overwritten.

An agent or skill turned off in orch is left as it is; orchd removes its files (see
[settings.md](settings.md)). The `exploit-finder` sandbox allow rule follows that agent's toggle:
added while it is on, removed while it is off. Third-party, unknown-origin and repo-specific skills stay out of the
binary as plain files in `~/.claude`.

**Items turned off in orch:** the settings edit reads the toggles from `orch.db` (read-only,
works with orchd down) and leaves out each orch item the user turned off: a disabled orch-hook
event is left as it is, a disabled orch allow or credential deny rule is not added, and the
summary names them. An unreadable store stops the plan instead of re-enabling them. See
[settings.md](settings.md#orch-install-and-drift). The tool hooks (`PreToolUse`, `CwdChanged`) are
the exception (D-40): a settings toggle cannot keep them out. Install reads the operator's
`hooks` [enforcement switch](daemon.md#enforcement-switches) from `orch.db` the same read-only
way and leaves them out only while it is `off`; a store it cannot read leaves them in.

### Safety net

`internal/safetynet` holds the pin (`Pinned`: version, repository URL, ref, commit, release date,
and the npm CLI version used only by an opt-in test), detection, and command planning. A release
bumps the pin deliberately, to a release at least 14 days old; a test checks the age.

**Locations.** For the real home: `CC_SAFETY_NET_HOME` (absolute) or `~/.cc-safety-net`, and the
marketplace under the orch data directory. A relocated `--home` uses `<home>/.cc-safety-net` and
`<home>/.local/share/orch/marketplaces/`, and runs `claude` with `HOME=<home>` and without
`CLAUDE_CONFIG_DIR` or `CC_SAFETY_NET_HOME`, so a test install cannot reach the real ones.

**Detection** reads `~/.claude/plugins/installed_plugins.json`, `known_marketplaces.json` and
`settings.json` `enabledPlugins` (none hold secrets; each read is capped at 1 MiB):

| State | When |
|---|---|
| `legacy` | a user- or project-scope install named `safety-net` or `cc-safety-net` from any marketplace other than `orch-pinned` (its hook would run twice) |
| `missing` | no user-scope `cc-safety-net@orch-pinned` |
| `outdated` | ours at another version, at another recorded commit, or registered from another directory |
| `disabled` | ours at the pin with `enabledPlugins` false |
| `ok` | otherwise |

Rules are ok when `rule.json` parses at version 1, lists `orch-guards`, has `rtk` in
`transparent_wrappers`, and the rulebook file equals the embedded one by meaning. Every listed
source is read for display: a bare name, or `owner/repo#ref/name` (its vendored copy), whose name
must match `^[a-zA-Z][a-zA-Z0-9_-]{0,63}$` before it becomes a path, which also rules out
traversal.

**Edits** (after the agents and skills): the marketplace manifest, generated from the pin; the
`orch-guards` rulebook, embedded from `internal/safetynet/orch-guards.rulebook.json` and
overwritten (orch owns it); then `rule.json`, merged: version 1, `orch-guards` appended to
`rules` and `rtk` to `transparent_wrappers` when absent, every other key, source, override and
wrapper kept. An unparseable `rule.json`, a version other than 1, or a non-array `rules` aborts
the plan instead of being rewritten.

**Steps.** A plan may also carry steps: commands run after every edit is written, planned from
the current state each time the plan is printed, counted or applied (a plan reused after Apply
sees nothing left), printed as `=== RUN ===`, counted as drift by `orch status`, and stopping at
the first failure. The Claude Code adapter plans `claude` commands, each an argument array with
empty stdin and a 3-minute timeout:

1. `plugin marketplace add <dir>` when `orch-pinned` is unknown; `marketplace remove orch-pinned`
   first when it points elsewhere (which also uninstalls its plugins); `marketplace update
   orch-pinned` when ours is outdated.
2. `plugin uninstall <id> --scope user` for each user-scope competing copy. Project- and
   local-scope copies are reported and left to the user.
3. `plugin install cc-safety-net@orch-pinned --scope user` when missing, or `plugin update ...`
   when outdated. `orch install` leaves a disabled plugin disabled; orchd's `InstallSafetyNet`
   runs `plugin enable`.
4. `plugin marketplace remove cc-marketplace` when it is registered and no other installed plugin
   (and no remaining project-scope copy) comes from it.
5. After an install or update, a check that Claude Code recorded the pinned commit: a missing or
   different `gitCommitSha` fails the plan. The plugin is not uninstalled then, since an unpinned
   copy still guards better than none.

Each command runs in its own process group, so a timeout also stops the git and ssh it started,
with a 5 s `WaitDelay` for anything still holding the output pipe. Only the last 4 KiB of output
is kept, with URL credentials and secret-shaped strings masked. Without a `claude` CLI, `orch
install` skips the plugin steps with a note, so `--target all` still wires the other harnesses;
`orch status` keeps reporting them.

`claude` is found as queued tasks find it (`PATH`, then the Homebrew and `~/.local/bin`
locations). Tests replace the runner with `internal/safetynet/fakeclaude`, which edits the same
files the real CLI writes; no test runs the real CLI. `TestRulebookWithPinnedCLI` runs every
rule case through the pinned analyzer's `explain --json` (it analyzes and executes nothing) when
`ORCH_CCSN_CLI` points at its `dist/bin/cc-safety-net.js`; it is skipped otherwise.

## Codex

- `~/.codex/config.toml`: `[mcp_servers.orch_rag]` and `[mcp_servers.orch_queue]` with the same
  command and args.
- `~/.codex/hooks.json`: same hook shape and events as Claude Code except `CwdChanged`, which
  Codex does not have, and without the `timeout` and `async` fields, which have not been checked
  against Codex's hook schema. Codex runs hooks only
  after the user reviews and trusts them.
- `~/.codex/AGENTS.md` (or a nonempty `AGENTS.override.md`, which Codex prefers): a managed block
  telling the agent to read the shared policy, since Codex does not expand `@` imports.

## Gemini

`~/.gemini/settings.json`: the same MCP servers, and hooks mapped `SessionStart`, `BeforeAgent`
(prompt), `SessionEnd`, `AfterTool`. `~/.gemini/GEMINI.md` imports the shared policy. Each
command gets the same per-event `timeout` as Claude Code, converted to milliseconds, the unit
Gemini CLI's hook configuration uses (default 60000): 10000, 10000, 15000, 5000. Gemini's hook
configuration documents only `type`, `command`, `name`, `timeout` and `description`, so no `async`
is written; Gemini does not wait for `SessionEnd` hooks at all (best effort), so that timeout only
bounds the process. Gemini's payload schema is assumed to match Claude Code's; this is not
verified.

## Tool hook coverage

Spaces (the command log and the tool guard) see a harness's tool calls only through its
`PreToolUse` hook, plus `CwdChanged` for Claude Code. `orch status` reads each harness's hooks
file and prints a `<harness> tool hooks` line under harness wiring: `wired` with the events,
`missing <events>` (a warning naming the `orch install` that fixes it), or, for Gemini,
`not covered by spaces`. Spaces cover Claude Code and Codex only (D-20), so the Gemini adapter
installs no tool hooks. A hook counts as wired only when its command is exactly
`<bin>/orch-hook <slug>`; a hook turned off by the operator's `hooks` enforcement switch reads as
missing.

## Common

- **Shared policy.** `internal/install/agent-instructions.md` is written to
  `~/.config/orch/AGENTS.md`, each `internal/install/modules/<name>.md` to
  `~/.config/orch/modules/<name>.md` (frontmatter stripped), and each
  `internal/install/policy/*.md` to `~/.config/orch/policy/`. orch owns these files. Every module
  file is written whether on or off; only the import decides whether agents load it. Harness
  entry files get a block between `<!-- BEGIN orchestrator-managed -->` and
  `<!-- END orchestrator-managed -->` containing `@<path>` for the shared policy and then each
  enabled module in `order` (Claude, Gemini), or a read directive listing the same files (Codex).
  Only the block is ever replaced; mismatched markers abort.
- **Instruction modules.** `install.Modules` lists the embedded files at build time: the file name
  is the ID, the frontmatter (`title`, `group`, `order`, `description`, optional `default`, strict
  keys, optional `sensitive`) feeds the settings UI. A module is on when this machine's `orch.db` says so, else per its
  `default` (on unless `default: false`). The block's edit reads the toggles the same way the
  settings edit does, so a module turned off stays out, is not drift, and an unreadable store
  stops the plan.
- **Shell alias.** `~/.zprofile` gets a marked block `alias orch="<bin>/orch"`.
- **LaunchAgent.** `~/Library/LaunchAgents/com.orchestrator.orchd.plist` (`RunAtLoad`,
  `KeepAlive`, logs under `~/Library/Logs/orchestrator/`), skipped when orchd is installed under
  Homebrew's `Cellar`, since `brew services` supervises it.

## Git hooks for outbound terms

`orch guard git-hook (--install|--uninstall) [--apply] [--repo DIR] [--hooks-dir DIR] [--orch BIN]`
(SP-045, `internal/install/githook.go`, `internal/cli/guard_githook_cmd.go`) puts a `commit-msg`
and a `pre-push` hook in a repository's hooks directory, which is what
`git rev-parse --git-path hooks` names (`core.hooksPath` when set, the common git directory's
`hooks` for a linked worktree), or in `--hooks-dir` (a shared `core.hooksPath`). It never edits
git config.

- **Preview and apply.** Without `--apply` it prints `DRY RUN`, then `CHANGE` or `UNCHANGED` per
  hook, and writes nothing. Apply re-reads each hook and refuses one that changed since the plan,
  backs up any hook it replaces or removes to `<data>/backups/git-hooks/<UTC timestamp>/` (as
  `orch install` does: 0700 directory, 0600 files), and writes the hook beside its place and
  renames it over, mode 0755. Re-running changes nothing; a hook of orch's that names another orch
  binary or lost its execute bit is rewritten in place.
- **Chaining.** A hook that is not orch's (no `# orch-git-hook:` marker line) moves, as it is (a
  symlink stays a symlink and its target is never read), to `<name>.orch-prev`, with a rename that
  fails rather than replace a file that appeared since the plan. orch's hook runs
  `<name>.orch-prev`, if executable, with the same arguments and, for `pre-push`, the same input:
  for `commit-msg` first, since such hooks add to the message (a Change-Id, a ticket prefix) and
  orch then checks the message as it will be committed; for `pre-push` after orch's check passes.
  When both `<name>` and `<name>.orch-prev` exist and `<name>` is not orch's, install refuses
  rather than lose one; a `<name>.orch-prev` left without a hook is named in the plan, since it
  will run again. A hook that is not a regular file or a symlink (a FIFO), or is over 1 MiB, is
  refused.
- **Uninstall** removes orch's hooks, moves `<name>.orch-prev` back, and leaves hooks that are not
  orch's alone, so the directory is as it was before the install.
- **The hook** is a POSIX `sh` script naming the orch binary that installed it (`--orch` to pick
  another); if that binary is gone it uses `orch` on `PATH`, and with neither it refuses and says
  how to remove itself. It runs `orch guard git-hook run commit-msg|pre-push -- <git's args>`.
- **The check** reads the terms layer's switch and the applied policy from orchd's guard snapshot,
  as the tool hook does (no orchd connection), and runs SP-044's outbound terms check
  ([hooks.md](hooks.md#outbound-terms)), with the operator's destination
  [outbound rules](hooks.md#outbound-rules) (SP-075; the baseline's switches are about reading
  shell and do not apply to what git really sends; rules that cannot be read refuse):
  `commit-msg` reads the message file and the staged diff
  (git sets `GIT_INDEX_FILE` for `commit -a` and `commit <paths>`, which the check keeps), with
  the checkout's push remote as the destination; `pre-push` takes the destination from the URL
  git passes (after `insteadOf`, `pushurl` and any `git -c` override, which the check's own git
  does not see) and reads the remote ref names it makes, the tag objects it pushes (by any ref
  name, tags of tags included) and the commits and tag messages of each ref update, less the
  objects the remote reports it holds and the tracking refs of remotes whose fetch URLs lie in the
  destination's spaces (tracking refs record what was fetched, not what a push URL holds; for a
  destination in no space, only remotes fetched from that same URL).
  Deletions send nothing. With the terms layer off it allows; with nothing ever
  applied there are no terms; an unreadable snapshot, a failing git or a check past 20 s refuses.
  A refusal names the term's space, line and hash, never the term.
- **Limits.** `git commit --no-verify` and `git push --no-verify` skip the hooks, and anyone who
  can write the hooks directory or set `core.hooksPath` can remove them; the hooks back up the
  tool hook for git run outside an agent's reach (scripts, the user's terminal), not against a
  committer who turns them off. The hook finds the guard files from its own environment
  (`ORCH_DATA_DIR`, `HOME`), so a git run with another home that holds no snapshot and no
  `orch.db` reads as nothing configured and allows (INV-10). Only git is covered; `gh` writes
  and the GitHub API are not. Installing into a `core.hooksPath` tracked in the work tree (husky,
  `.githooks`) changes tracked files and writes the local orch path into them.

## Harness sandbox and permission layering

These are the measured facts that per-space harness settings (spaces plan, SP-046) build on.
They were recorded on 2026-11-08 with Claude Code 2.1.309 and codex-cli 0.166.0 on macOS, in a
scratch directory, without changing any real harness configuration (spaces plan, SP-004).

**Claude Code.** Headless `claude -p` sessions ran in scratch git repositories. Each settings
file set a distinct `env` marker, and the run recorded which markers reached the Bash tool:

| Launched from | User | Root `settings.json` | Root `settings.local.json` | Subfolder's own `.claude/` |
|---|---|---|---|---|
| checkout root | yes | yes | yes | n/a |
| subfolder with its own `.claude/settings{,.local}.json` | yes | **no** | yes | both apply |
| subfolder without `.claude/` (two repositories) | yes | **no** | yes | n/a |
| subfolder of a non-git directory whose parent has `.claude/` | yes | n/a | n/a | parent **not** applied |

- A project `.claude/settings.json` applies only when the session starts in its directory, but
  the checkout root's `settings.local.json` applies anywhere in the checkout. Per-checkout files
  therefore belong in the root `settings.local.json`. Because the root `settings.json` may not
  apply, that file should carry every key the deny depends on: `sandbox.enabled: true`,
  `sandbox.allowUnsandboxedCommands: false` and the `Read(//...)` denies. This follows from the
  runs below; no run launched from a subfolder with all three in the local file alone.
- `sandbox.enabled: true` in project settings overrides `enabled: false` in user settings.
  `sandbox.filesystem.denyRead` entries from project and local settings merge: both folders were
  denied in one session.
- With the sandbox on and `allowUnsandboxedCommands: false`, `denyRead` makes `cat` fail with
  `Operation not permitted`. It does **not** stop the Read tool, which returned the file.
- A `permissions.deny` rule `Read(//<absolute path>/**)` blocks the Read tool ("File is in a
  directory that is denied by your permission settings"). With the sandbox on and
  `allowUnsandboxedCommands: false`, the same rule also makes sandboxed `cat` fail, so one Read
  deny covered both. Grep, Glob, Edit, Write and other file tools were not tested against it. The
  single-slash form `Read(/<absolute path>/**)` did not block (presumably because it is resolved
  against the settings file).
- A fresh `CLAUDE_CONFIG_DIR`, or a temporary `HOME`, starts logged out (`Not logged in · Please
  run /login`). A space that names its own `claude_config_dir` for `orch run` needs its own login
  there (D-59; spaces that name none use your own). Whether that login then persists per folder
  was not tested.
- In auto permission mode, the auto-mode classifier can refuse a Bash call before the sandbox is
  reached. Tests of the sandbox itself must use `--permission-mode default`.

**Codex.** Codex 0.166 can deny reads to a folder from shell commands with a permissions profile:

```toml
default_permissions = "job"
[permissions.job]
extends = ":workspace"
[permissions.job.filesystem]
"/abs/other-job-root" = "deny"
```

- Under `codex sandbox` (the same seatbelt sandbox, with no model), the profile made `cat`, `ls`,
  Python `open()`, a symlink from the workspace and `cd` into the folder all fail. Without the
  profile, the same `cat` succeeded.
- The profile takes effect from `$CODEX_HOME/config.toml`, from `-p <name>`
  (`$CODEX_HOME/<name>.config.toml` layered on the user config), or from `-c` overrides. In a
  `codex exec` session with the profile given by `-c`, the session ran under it
  (`sandbox: workspace-write`) and the agent declined the read, citing the denied path.
- A checkout's own `.codex/config.toml` cannot make it the session's profile. Even with the
  project trusted, `codex exec` from the root and from a subfolder ran `sandbox: read-only` and
  read the denied file. (`codex sandbox -P job` does apply a profile defined there, but
  `codex exec` has no such flag.) Codex has no per-checkout path for this, so per-space Codex
  settings need the launcher (`-p <space>` or `-c`), not a per-checkout file. This also means a
  checkout cannot weaken a profile the launcher supplies.
- The `:workspace` base still lets the shell write `/tmp` and `$TMPDIR`, which every space
  shares; a read deny on other roots does not close that channel.

## Per-space harness settings

Spaces plan SP-046 (D-23, D-53) puts each space's OS sandbox and file-tool denies, its gh profile
and its own temporary folder into the sessions that work for it, two ways. Code:
`internal/settings/spacesettings`, `internal/daemon/checkout_settings.go`, `internal/cli/run_cmd.go`.

**Rules for a space** (`spacesettings.For`). A session in space A is denied, for both reading and
writing, every root of every other isolated space, every other space's folder under
`<data dir>/spaces/` and its temp folder, every other space's `gh_config_dir` (it can hold that
account's token, and `GH_CONFIG_DIR` pointed at it acts as that account) and `claude_config_dir`
(its login and transcripts), and `<data dir>/spaces/.orch/`, orchd's record of the checkout files and their
backups. Nothing of A, of a public space or of a private space is denied; memory filtering and the tool guard cover those, as
in `block-other-jobs` (D-12). A folder reached through a link is denied by its link path and its
real path. The space's environment is `GH_CONFIG_DIR` (its `gh_config_dir`, only when it names
one; otherwise the variable is not set and gh uses the default login, D-59), `TMPDIR` and `CLAUDE_CODE_TMPDIR`, both the space's temp folder `/tmp/orch-<uid>/<space>`
(`/tmp/orch-<uid>/_<hash>` for a name too long; the root and the folder are created `0700`, and a
root that is a link or another user's is refused: orchd then reports it and the space's checkout
settings name no temp folder at all, rather than one another user controls). A checkout with no space (its root and its remote name different
jobs, or it is quarantined) is denied every isolated root that does not hold it, every space's
folder, gh profile and Claude Code folder, the default gh folder and `.orch/`, and gets no environment.

*gh logins inside the sandbox (D-61, RC-208 option a).* A space reads only its own gh login. One
naming no `gh_config_dir` (the default login, D-59) gets the default gh folder (orchd's
`$GH_CONFIG_DIR`, else `$XDG_CONFIG_HOME/gh`, else `~/.config/gh`; `spacesettings.DefaultGHDir`)
in `sandbox.filesystem.allowRead`; one naming its own profile gets that profile there and is
denied the default folder; every other space's profile stays denied to both. An `allowRead` that
would hold a denied folder (another space's profile inside `~/.config/gh`) is left out, as it
would re-open it. Reason: `orch install` puts `Read(~/.config/gh/**)` in `~/.claude/settings.json`
to keep the file tools off gh's files, and Claude Code applies a Read deny to sandboxed commands
too, so without the `allowRead` gh could not start in the shell. A placed space's rules also let
gh work in the sandbox: `api.github.com` and `github.com` in `sandbox.network.allowedDomains`
(otherwise each connection needs an approval a headless session cannot give) and
`sandbox.enableWeakerNetworkIsolation: true` (see the measurements below). The guard's
self-protection (SP-042) still refuses any tool call or shell command that names a file in a gh
profile, the default one included, so the folder is readable to gh, not by name.

In Claude Code settings this is `sandbox.enabled: true`, `sandbox.allowUnsandboxedCommands: false`,
each denied folder in `sandbox.filesystem.denyRead` and `denyWrite` and as `Read(//<path>/**)` and
`Edit(//<path>/**)` in `permissions.deny` (Read rules also cover Grep and Glob, Edit rules Write
and the other edit tools), the space's temp folder in `sandbox.filesystem.allowWrite`, its gh
login folder in `sandbox.filesystem.allowRead`, the gh hosts in `sandbox.network.allowedDomains`
and `sandbox.enableWeakerNetworkIsolation: true` (the last two for a placed space only),
`enableAllProjectMcpServers: false` (a project `.mcp.json`, which the session's file tools can
write, would otherwise start servers outside the sandbox when every project server is enabled),
and the environment in `env`. A path holding a character Claude Code's rules read as a pattern
(`*?[]{}()!\`) gets no permission rule; the sandbox still denies it to shell commands.

**gh in the sandbox (measured, Claude Code 2.1.309, 2026-11-14).** Sessions started in a temp
checkout with the user's real `~/.claude/settings.json` (which holds `Read(~/.config/gh/**)`) and a
`settings.local.json` with `sandbox.enabled: true` and `allowUnsandboxedCommands: false`, running
`gh auth status` in the sandboxed shell:

| `settings.local.json` adds | `gh auth status` in the sandbox |
|---|---|
| nothing | `failed to read configuration: open ~/.config/gh/config.yml: operation not permitted` |
| `filesystem.allowRead: ["~/.config/gh"]` (or the absolute path) | config read, token read from the keyring, then `deny network-outbound api.github.com:443 (user denied)` |
| and `network.allowedDomains: ["api.github.com", "github.com"]` | `tls: failed to verify certificate: x509: OSStatus -26276` (curl to the same host: HTTP 200) |
| and `enableWeakerNetworkIsolation: true` (or `network.allowMachLookup: ["com.apple.trustd.agent"]`) | `Logged in to github.com account <login> (keyring)`, exit 0 |

So an `allowRead` overrides a permission-rule Read deny for sandboxed commands; gh reaches the
login keychain from the sandbox without any extra setting; and Go programs need the
`com.apple.trustd.agent` lookup to verify certificates. Claude Code describes
`enableWeakerNetworkIsolation` as opening "a potential data exfiltration vector through the trustd
service" (RC-213). All three keys are honoured from a checkout's `settings.local.json` and from
`--settings`. `TestClaudeE2E`'s `gh`, `launcher/gh`, `gh-guard` and `two-logins` probes check it in
real sessions.

**Temp folders (measured, Claude Code 2.1.309).** Claude Code gives sandboxed commands
`TMPDIR=$CLAUDE_CODE_TMPDIR/claude-<uid>`, and `/tmp/claude-<uid>` (shared by every session of the
user) when that path is longer than 44 bytes, whatever `TMPDIR` says. Only the environment Claude
Code starts with sets it: `CLAUDE_CODE_TMPDIR` or `TMPDIR` in a settings file's `env` changes
neither the sandboxed `TMPDIR` nor the `CLAUDE_CODE_TMPDIR` the shell sees (other `env` entries,
`GH_CONFIG_DIR` included, do reach the shell), and no `sandbox` setting names a temp folder. That
is why the temp folders sit under `/tmp` rather than in orch's data directory, which is too long.
So `orch run` gives a session its space's own sandboxed `TMPDIR`, and its rules also deny the
shared `/tmp/claude-<uid>`; a session started in a checkout without `orch run` uses the shared
folder, which every other such session can read, whatever its space. That is accepted as a
documented limit until Claude Code honours a settings value for it (D-58, which resolved RC-206). Denying that folder
in a checkout's settings would break every command that reads back a temp file (`mktemp` failed
with `Operation not permitted`), so orch does not. Every space's settings still deny every other
space's temp folder, so a file a launched session writes under its `TMPDIR` is unreadable from
other spaces' sessions however they started.

**Per-checkout files.** orchd keeps `.claude/settings.local.json` in every git checkout (a folder
with a `.git` folder or file) under a space's roots. SP-004 measured that the checkout root's
`settings.local.json` applies wherever in the checkout a session starts, while its `settings.json`
may not, so every key is written there together. Each checkout's file also denies shell writes to
the checkout's own `.claude` folder (`denyWrite`) and file-tool edits of its `settings.json` and
`settings.local.json` (`Edit(//...)`), so the session cannot change or replace the settings that
hold its rules. (Claude Code 2.1.309's sandbox already denies shell writes to those two files
and to `.claude/skills`, `agents`, `commands` and `hooks` in a writable folder.)

- *Discovery:* every root of the applied policy is walked at most 6 folders deep, not into a
  checkout below the root (a root that is itself a checkout is walked), a hidden folder, `node_modules`, `vendor`, `Library`, `__pycache__`, `target`, `dist`
  or `build`, and not through links below the root; at most 50,000 folders per sync. On the
  developer's Mac, `~/code` (135 checkouts) took 0.38 s to walk.
- *Placement:* each checkout is placed as content from it is (its deepest root and its remote's
  owner, `placementOrigin` and `PlaceUnder`), but gets a space's rules only when its folder root
  decided that: by itself, with the remote agreeing, or being the stricter. A checkout only its
  remote places (say a job's repo cloned under a personal root) gets the rules of a checkout with
  no space. Reason: the remote comes from `.git/config`, which the session in the checkout can
  rewrite, and for deny rules a stricter space would unlock that space's folders and gh profile.
- *Fail closed:* a checkout orch wrote keeps its rules while it is still a folder under a root,
  even when a sync does not find it (its `.git` renamed, or the walk cut short) or cannot place
  it; it gets its last space's rules, or a checkout with no space's when that space is gone. A
  new checkout that cannot be placed gets a checkout with no space's rules. Only the `sandbox`
  switch, the policy (a root removed) or the folder going takes orch's entries out.
- *When:* at orchd start, every 30 s, at once after `ApplySpaces`, and at once when the
  `sandbox` switch or the sandbox setting changes. A new clone gets its file within one interval plus one sync.
- *Merge:* orch records in `<data dir>/spaces/.orch/checkouts.json` (`0600`) exactly what it added to
  each file: list entries the file did not already hold, values it set with the value before, and
  objects it created. A later sync takes out only those (an entry the user had before stays,
  even when orch wants the same one), sets values back as they were unless the user changed them
  since, and removes a file orch created once nothing is left in it. Other keys and the user's
  own entries are never touched. A second sync with nothing changed writes nothing. The file as it
  was before orch's first change is kept once in `<data dir>/spaces/.orch/backups/<hash>/`
  (`0600`, with the checkout's path beside it).
- *Unusable entries:* a `.claude` that is a link or not a folder, or a settings file that is a
  link, not a regular file or over 4 MiB, is renamed to `<name>.orch-moved-<time>` beside it (the
  link itself, never what it points at) and orch's file written; this is reported, and the report
  is the only record of where it went (switching off does not move it back). An entry git tracks
  is never moved, and nothing is moved when git cannot say. With the switch off nothing is moved.
- *Unmergeable file:* a file orch cannot merge into (not JSON, or a key orch writes holding the
  wrong kind of value) is kept as `invalid-<time>.json` beside the backup and replaced by one
  holding only orch's rules, which orch then owns; this is reported. With the switch off it is
  only reported.
- *Writes:* the file is written beside itself and renamed over, after reading it again; when
  Claude Code (saving a permission) or anyone else changed it meanwhile, nothing is written and
  the next sync tries again. `.claude` and the file are opened without following links, so a
  session cannot make orchd write elsewhere by replacing them with links.
- *Git:* before the file holds anything of orch's, `git check-ignore` must report it ignored.
  When no rule covers it orch appends `/.claude/settings.local.json` to the checkout's
  `info/exclude` (`git rev-parse --git-path`, shared by a worktree's main checkout); it never
  edits a `.gitignore`. It writes only a file named `exclude` in a folder `info` of a folder
  holding `HEAD`, opening each without following links and creating only `info`. A checkout whose file is tracked is refused and reported. git runs with
  `core.fsmonitor` and the untracked cache off, since a checkout's `.git/config` is writable by
  the session in it.
- *Switch:* with the `sandbox` layer off, or no policy applied, every sync takes orch's entries out
  of every file it wrote and leaves the user's; switched on, they come back at the next sync. While
  the applied policy does not verify, files are left as the last good policy wrote them.
- *Setting:* where the sandbox setting writes no rules (below), a checkout keeps only
  `env.GH_CONFIG_DIR` for a space naming its own `gh_config_dir` (which account it uses, D-59, not
  a sandbox rule) and nothing for one on the default login; no temp folder is made for it.
- Problems (a tracked or unmergeable file, a link, a placement error) are logged by orchd when
  they change.

**Launcher.** `orch run <space> -- claude|codex [args...]` starts a session with the space's
profile from any folder. It reads the applied policy through `DiffSpaces` (operator-only), so it
works in your own terminal and not inside an agent session. It refuses a space not in the policy,
and a folder another space holds or whose space cannot be decided unless `--force`; a folder no
root or remote places is allowed. It then replaces itself with the harness (`exec`), with the
variables set for the harness only:

- *Claude Code:* the space's rules (the per-checkout settings without the `.claude` protections,
  plus denies of the shared `/tmp/claude-<uid>`) as one compact JSON string in `--settings`,
  before your arguments; no file holds them. Claude Code adds flag settings to your own
  (`$CLAUDE_CONFIG_DIR` or `~/.claude`) and the project's, merging the lists, so your hooks,
  instructions and login carry over. `CLAUDE_CONFIG_DIR` is set only when the space names its own
  `claude_config_dir` (D-59); otherwise the session uses your own configuration and login.
  Because Claude Code takes one `--settings`, the launcher refuses one of yours while the rules
  apply.
- *Codex:* `-c default_permissions="orch_<space>"` and `-c permissions.orch_<space>={extends=
  ":workspace",filesystem={"<path>"="deny",...}}` before your arguments, with your own
  `CODEX_HOME` and login. A checkout's `.codex/config.toml` cannot change it (SP-004). Codex has no
  per-checkout settings path, so the launcher is how a Codex session gets the denies.
- *Environment:* `TMPDIR` and `CLAUDE_CODE_TMPDIR` are always the space's. `GH_CONFIG_DIR` and
  (for Claude Code) `CLAUDE_CONFIG_DIR` are set only when the space names a `gh_config_dir` or
  `claude_config_dir`, replacing inherited values, and then the inherited variables that would
  override that login are dropped with a note naming each (never its value): gh's `GH_TOKEN`,
  `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN` and `GH_HOST`, as orchd's own
  gh calls drop them, and Claude Code's `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and
  `CLAUDE_CODE_OAUTH_TOKEN`. Otherwise an inherited value is your own login and is passed on,
  unless it names a folder another space names (compared case-insensitively, made absolute and
  with links resolved), which is dropped with a note so the session cannot run as that space's
  account.
- With the `sandbox` layer off, or the sandbox setting giving this space no rules, the session gets
  no deny rules but keeps the environment, and the launcher says which. When orchd cannot report
  the switch and the setting, the rules apply.

**Sandbox setting (D-61).** Whether orch writes its sandbox rules is the operator's lasting choice,
`orch spaces sandbox auto|on|off` (operator-only, refused inside agent sessions and orchd's
tasks; stored in orch.db, each change logged with its time and caller; see
[daemon.md](daemon.md#sandbox-setting)):

- `auto` (the default): rules only for a session they keep out of another isolated space's root
  (`Rules.OtherJobs`): a checkout of a space when another isolated space has a root, a checkout of
  no space when an isolated root does not hold it; or out of another space's own login folder,
  its `gh_config_dir` or `claude_config_dir`, whatever that space's kind, or out of the default
  gh login's folder, which a space naming its own `gh_config_dir` and a checkout of no space are
  denied (`Rules.OtherLogins`, D-69). A policy with one isolated job on the default logins and
  nothing else writes none; add a second job, a public space whose checkouts must stay out of
  the job, a space naming its own login folder, or give the job its own gh profile, and they
  come.
- `on`: rules for every space's sessions while a policy is applied.
- `off`: none, by choice.

The setting applies to checkouts and `orch run` alike (`spacesettings.Choice`). Without the rules
the per-space `TMPDIR` is not written into checkouts (Claude Code ignores it there anyway, D-58),
a space's own `GH_CONFIG_DIR` stays, and `orch run` still sets the space's temp folder and login
variables. The tool guard, memory filtering and outbound terms are unaffected: this applies to the
sandbox layer only (D-11 and INV-16 hold for the others). `orch status`, `orch spaces sandbox`,
`orch spaces enforce` and the app (Status and GetEnforcement carry it) show the setting and
whether any space is sandboxed now; unlike the `sandbox` switch it never marks a session's start
digest as enforcement off (INV-16 as amended). A session's next start digest names it once after
each change to it, and after each orchd start while it is off (D-68,
[daemon.md](daemon.md#sandbox-setting)). The SP-070 `sandbox` switch stays for debugging: switched off (timed
or not) it takes every entry out, the gh profile included, and every digest says so.

**Logins (D-59).** One login works by default: a space that names neither `gh_config_dir` nor
`claude_config_dir` uses whichever gh login is already authenticated and your normal Claude Code
configuration, in checkouts and with `orch run`, and needs no setup. The trade-off is that the
GitHub account is then not a boundary between jobs; the guard, memory spaces and the folder
denies still are. A space may name its own:

- `gh_config_dir`: log in to it once with `GH_CONFIG_DIR=<gh_config_dir> gh auth login`.
- `claude_config_dir` (a folder outside every git tree, not shared with another space): only
  this needs a separate Claude login. SP-004 measured that a fresh `CLAUDE_CONFIG_DIR` starts
  logged out (`Not logged in · Please run /login`), so log in once with
  `orch run <space> -- claude` and `/login` in it, or `CLAUDE_CONFIG_DIR=<claude_config_dir>
  claude`. Whether that login then persists per folder was not measured (RC-191). orch does not
  write into the folder: its settings, hooks, instructions and plugins are whatever you put
  there (copy or link them from `~/.claude`), and the space's rules still come from
  `--settings`.

orch never handles either token. Showing which login each space will use is SP-074.3.

**Limits.**
- Root paths of other jobs are written into every checkout's settings, so a session can read
  which folders are denied (not what they hold). Name job roots neutrally. The launcher's
  `--settings` JSON carries the same paths in the process arguments, which other local users
  can see with `ps`.
- Denying another space's `gh_config_dir` or `claude_config_dir` covers what is in the folder,
  not the macOS login keychain, where gh keeps its token by default (`gh auth login` without
  `--insecure-storage`) and Claude Code keeps its login. Measured 2026-11-14 (Claude Code
  2.1.309, `TestClaudeE2E` `two-logins/keychain`, account metadata only): a session of a space
  naming its own gh profile wrote a profile folder of its own naming the default account, and
  `GH_CONFIG_DIR=<it> gh auth status` in its sandboxed shell reported that account logged in
  from the keyring. So any sandboxed session can use any gh login whose token is in the keychain,
  by naming its account. Adding `~/Library/Keychains` to a session's `denyRead` stopped gh
  reaching the keyring (`The token in default is invalid`), but also breaks every keyring login
  in that session. Which mitigation to take is open (RC-210). A per-space login is only as
  separate as where its token is stored.
- A space on the default login has its gh folder readable by every sandboxed command (D-61): with
  gh's default keyring storage `hosts.yml` names the account but holds no token; with
  `--insecure-storage` it holds the token. The guard refuses commands and tools that name the
  files, not a program that opens them itself.
- `sandbox.enableWeakerNetworkIsolation` lets sandboxed commands look up
  `com.apple.trustd.agent`, which Claude Code calls a potential exfiltration path; it is what
  lets gh (and other Go programs) verify certificates in the sandbox (RC-213). Claude Code
  2.1.309's own messages say a machine whose sandbox has such a setting serves no "device tools"
  (read from its messages, not measured).
- `sandbox.network.allowedDomains` lets every sandboxed command, not only gh, reach
  `github.com` and `api.github.com` without an approval; the outbound terms check (SP-044) reads
  `gh` and `git` commands, not other programs. A GitHub Enterprise host in a space's own profile
  is not added (RC-214).
- Claude Code's settings files with hooks (`~/.claude/settings*.json` and a checkout's
  `.claude/settings*.json`) are not denied to the file tools by the space rules; the guard's
  self-protection (SP-042), which refuses writes to them, is the protection (D-61, RC-211).
- An `allowRead` or `allowWrite` that orch did not write re-opens orch's denies below it.
  Measured 2026-11-14 (Claude Code 2.1.309): with orch-style `denyRead` of a folder in a
  checkout's `settings.local.json`, a sandboxed `cat` of a file in it printed the file when the
  checkout's own `.claude/settings.json` (a repository can commit one) or the same
  `settings.local.json` held `sandbox.filesystem.allowRead` of that folder; a `settings.json`
  `excludedCommands: ["cat"]` did not. In a session started as `orch run` starts it, whose
  denies come from `--settings`, the same `settings.json` in the launch folder did not re-open
  them. orch cannot change a repository's file and keeps the user's own entries, so each sync
  reports such an entry, and any settings file or `.claude` it cannot check (a link, not a
  regular file, over 4 MiB, not JSON), as a problem in orchd's log, and `orch run` prints it as
  a warning (`spacesettings.Loosening`, which reads only the checkout's own two files, without
  following links). The tool guard refuses tool calls there (D-67, rule `settings_reopen`,
  [hooks.md](hooks.md#enforcing-guard)): each sync records in `.orch/checkouts.json` the folders
  a checkout's file denies while it holds orch's sandbox rules, and the guard reads every
  `.claude` folder from the call's `cwd` and the session's project folder
  (`CLAUDE_PROJECT_DIR`) up to such a checkout (`spacesettings.Reopened`); an entry that
  re-opens one of them, or a file there it cannot check, refuses the call until it goes
  (`TestClaudeE2E` `loosening`). A subfolder that is gone or is a link is skipped (its target
  is checked as the folder's real path); the checkout's own folder is always checked.
- The same project file could otherwise turn the guard off. Measured 2026-11-14 (Claude Code
  2.1.309, `TestClaudeE2E` `hook-pin` and a measurement before it): a project `settings.json`
  with `"disableAllHooks": true` turns off every hook, those given by `--settings` included,
  and its `env` reaches the hooks over the environment Claude Code started with, so an
  `ORCH_DATA_DIR` there would point the guard at an empty folder, where it allows everything as
  `no_policy`; in both cases a `settings.local.json` value wins. So orch's rules also write
  `"disableAllHooks": false` and `env.ORCH_DATA_DIR` (orchd's data folder) into the checkout's
  file and the launcher's `--settings`, and `Loosening` reports, and the guard refuses on, a
  `disableAllHooks: true` or an `env` entry for `ORCH_DATA_DIR`, `ORCH_CONFIG_DIR` or
  `ORCH_SOCKET` in the checkout's own files that orch did not write. Measured the same day
  (`TestClaudeE2E` `permission-rules`): a project `permissions.allow` `Edit(//<root>/**)` or
  `Read(//<root>/**)`, or `permissions.additionalDirectories` naming another job's root, does
  not re-open the sandbox (a sandboxed write and read with paths built at run time both
  failed), so those keys are not checked.
- Limits of the check: it does not stop the sandboxed commands themselves (only tool calls,
  and every shell command is one); it judges the files as they are at each call; a checkout
  that gained orch's rules less than one sync ago (up to 30 s) is not yet in the record; folders
  above the checkout are not read (which ancestors Claude Code loads was not measured), and a
  folder spelled in another Unicode normalization than the record is not matched (RC-652). The
  user's own `~/.claude` settings, managed settings and other `--settings` files merge the
  same way and are not checked. The file-tool `Read(//...)` denies still hold.
- `auto` also writes the rules when they would deny another space's own login folder (D-69),
  but not for `.orch/` alone: with one isolated job and spaces naming no login folder, the job's
  sessions get no rules, so nothing in the sandbox denies them `.orch/` (D-69 chose login
  folders, not `.orch/`).
- Deny paths are written as the policy spells them, plus their real path; a root spelled in
  another case than on disk relies on the sandbox matching case-insensitively, which was not
  measured (RC-218).
- Paths match by case in Claude Code's permission rules; macOS volumes are usually not case
  sensitive, so a tool given a differently cased path may pass the `Read` rule (the sandbox
  matches the real path).
- The shared `/tmp` stays writable by every space (D-53); only `TMPDIR` is per space, and for
  Claude Code only in sessions started with `orch run` (sessions started in a checkout share
  `/tmp/claude-<uid>`, a documented limit under D-58).
- With the default logins (D-59), every space's shell runs gh as the same account, and every
  session started without a `claude_config_dir` shares one Claude Code configuration, its
  transcripts under `~/.claude/projects` included.
- `/tmp` is cleaned by the system; orch makes the temp folders again at each sync and launch.
- Harness transcripts under `~/.claude/projects` and `~/.codex/sessions` are shared by sessions
  started without the launcher and are not denied.
- A settings file a session edits is put right at the next sync (30 s), but a running session
  may already have read it.
- A checkout seen as ignored is not checked again until orch changes its file.
- The session in a checkout controls its `.git`, so it can point the `info/exclude` orch appends
  to at another repository's git folder; orch then adds its one ignore line there.
- A `.claude` the user linked on purpose (a shared folder) is moved aside like any other link.
- A moved-aside name is not git-ignored, so `git add -A` could pick it up.
- A checkout whose settings file is tracked by git (a cloned repo can commit one) gets no rules:
  orch never writes a tracked file, and reports it. A session in a checkout orch already wrote can
  track the file with `git add -f`; its rules then stay as they are but no longer follow policy
  changes. Refusing sessions in such a checkout is the enforcing guard's (SP-041).
- The `Edit(//...)` denies on the checkout's settings files match the path a tool names. Whether
  Claude Code resolves links and case before matching was not measured; the opt-in Claude Code
  test's `protect` probe checks it.
- The deny rules are only as complete as the folders they name: orch's data folder outside
  `spaces/` (`orch.db`, the command log) stays readable to every space, and the default gh login
  folder to every space that uses it (it is denied to spaces naming their own profile and to
  checkouts of no space); self-protection (SP-042) covers orch's own files. A space's
  `claude_config_dir` is denied to other spaces' sessions, but the guard's self-protection does
  not treat its files as credentials as it does a gh profile's.
- Shell commands in a checkout cannot write its `.claude` folder, so a `git checkout` or `git pull`
  run by the session that changes tracked files under `.claude/` fails there; the file tools
  still edit them, and the user's own terminal is not sandboxed.
- Gemini is not covered (D-20).

## One-command install

`orch install` with no `--target` (`auto`) builds `InstallPlan` over `FullUnits` for every harness
whose folder exists under the home (`~/.claude`, `~/.codex`, `~/.gemini`; Claude Code when none
does), and `--target all` for all three. It prints one plan, then applies it with `--apply` or
`--yes`, or on a `y` at a `[y/N]` prompt when stdin is a terminal (a non-terminal never applies
without a flag). A single `--target claude-code|codex|gemini` builds only that adapter's units,
with the same output as earlier releases. Backups go to `<data>/backups`, where `<data>` is
`<home>/.local/share/orch` for a relocated `--home`: one that is not the real home, compared with
links followed (`safetynet.SameDir`), so a symlink to the real home or another spelling of it is
not relocated.

The machine units (`internal/install/machine.go`), each skipped by its flag:

- **Model** (`--no-model`): download into `embedder.model_dir` (default `<data>/models/minilm`)
  when it is not present. Its undo is empty: the model is data (see purge).
- **Git hooks** (`--no-git-hooks`): `orch guard git-hook --install --apply` records the hooks
  directory in `<data>/git-hooks.json` (0600, absolute paths only) before writing the hooks, and
  `--uninstall --apply` drops it after removing them. Install plans `PlanGitHooks` only for a
  recorded directory that still holds one of orch's hooks, and repairs drift (another orch path, a
  lost execute bit), so an entry alone never makes install write hooks; uninstall removes orch's
  hooks there, puts back `<name>.orch-prev`, and drops the entry (also for a directory that is
  gone).
- **Daemon** (`--no-daemon`): when orchd does not answer, `brew services start orch` (Homebrew
  install, orchd under a `Cellar`) or `launchctl load -w <plist>`. Uninstall runs `brew services
  stop orch` while orchd answers, or `launchctl unload -w <plist>` (`launchctl remove` without a
  plist) while `launchctl list com.orchestrator.orchd` finds it. A relocated home never runs
  launchctl or brew. Commands run as argument arrays through `CommandRunner`, with a 2-minute
  timeout; tests inject a fake.
- **App:** install only names the cask when no `Orch.app` is in `/Applications` or
  `~/Applications` (`--no-app` drops it). Uninstall names what to do for an installed app.

## Uninstall

`orch uninstall [--apply|--yes] [--target all|claude-code|codex|gemini] [--home DIR] [--bin-dir DIR]
[--no-git-hooks] [--purge-data]` (`internal/cli/uninstall_cmd.go`) builds `UninstallPlan`.

- **Target.** `all` (the default) walks every registered unit. One harness walks only its
  adapter's units, less the shared ones, so the other harnesses keep the shared policy, alias and
  launchd agent; it takes no machine units and refuses `--purge-data`.
- **Preview:** `DRY RUN — uninstall`, then each step as `=== RUN ===`, then `=== MODIFY|REMOVE
  <path> ===`, each empty directory to remove, and `Left for you to do by hand`. Files with
  nothing of orch's are not listed.
- **Apply:** run the undo steps, then measure the edits again (a step such as `claude plugin
  uninstall` edits files the edits read), then for each changed file: re-read and refuse one that
  changed since it was measured, back it up as install does, and write it or remove it. Removal
  re-checks with `Lstat` that the path is still a regular file, and `os.Remove` never follows a
  final symlink; a symlink is never removed (when only orch's part was in the file behind it, that
  file is emptied). Directories in `Dirs` are removed only when empty and not symlinks.
- **Steps, in order:** stop orchd (daemon unit), remove git hooks, `claude plugin uninstall
  cc-safety-net@orch-pinned --scope user` and `claude plugin marketplace remove orch-pinned` (only
  what `Detect` finds; skipped with the command named when there is no `claude` CLI), then the
  spaces checkout settings: a `spacesettings.Syncer` over `<data>/spaces/.orch/checkouts.json`,
  switched off, takes out exactly the entries orch recorded and removes files it created (orchd is
  stopped first so it does not put them back), then the purge.
- **`rule.json`:** `safetynet.UnmergeRuleConfig` drops `orch-guards` from `rules` and `rtk` from
  `transparent_wrappers` (each list removed once empty) and removes a file left as
  `{"version": 1}`.
- **Purge** (`--purge-data`, `internal/install/purge.go`): one step per path, listed in the
  preview: every entry of the data directory except `backups/`, `~/Library/Logs/orchestrator/`,
  and `config.yaml`/`rag-eval.yaml` in the config directory. The CLI then needs `--yes`, or the
  word `delete` typed at a terminal; `--apply` alone from a script refuses. Exactly the listed paths
  are deleted (`Machine.PurgeList`), never one that appears after the confirmation. The data
  directory must be absolute, not `/`, the home or a folder holding it, not a symlink, and either
  `<home>/.local/share/orch` or hold `orch.db` (`ORCH_DATA_DIR` can point anywhere). Each path is
  removed through `os.Root` opened on its parent, so a symlink inside is removed as a link and
  never followed.
- **Left for you:** Homebrew (`brew uninstall orch`) or the source-built binaries, Orch.app and its
  login item (`SMAppService.mainApp`, which only the app can unregister), git hooks installed before
  the registry existed, the kept data directory, and the backups.
- **Not exact:** files are written back in orch's encoding, so a harness's own formatting or a TOML
  comment is not restored; a rule, wrapper or `attribution.commit: ""` the user had before install
  is removed with orch's (install did not record it); a missing final newline before orch's block
  comes back as one; the `info/exclude` line for checkout settings stays.

## Status checks

- **Drift:** each adapter's pending edits. Items turned off in orch are not drift; the
  `claude-code` line reports `N disabled by you` (instruction modules included).
- **Instruction footprint:** for each harness whose entry file exists, follow `@` imports (depth 5,
  each file once; for Codex, the shared policy and each module path it lists) and report bytes and estimated tokens (`(bytes+3)/4`), the context cost of every
  session.
- **Repository instructions:** inside this repo, `AGENTS.md`, `CLAUDE.md` and the shared policy
  are compared with `HEAD`, and `CLAUDE.md` must be exactly `@AGENTS.md`.
