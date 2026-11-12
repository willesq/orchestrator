# Install and harness adapters

`orch install [--apply] [--target claude-code|codex|gemini|all] [--home DIR] [--bin-dir DIR]`
wires orch into a harness. It previews by default; `--home` relocates everything for testing.

## Edit model

Each adapter returns a plan of edits: `{Path, Compute(old) → new, summary, Normalize, Unlink}`.

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
  run /login`). A per-space profile for `orch run` needs its own login. Whether that login then
  persists per profile was not tested.
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
`<data dir>/spaces/` (which holds each space's `tmp/` and its `claude/` profile), every other
space's `gh_config_dir` (it can hold that account's token, and `GH_CONFIG_DIR` pointed at it acts
as that account), and `<data dir>/spaces/.orch/`, orchd's record of the checkout files and their
backups. Nothing of A, of a public space or of a private space is denied; memory filtering and the tool guard cover those, as
in `block-other-jobs` (D-12). A folder reached through a link is denied by its link path and its
real path. The space's environment is `GH_CONFIG_DIR` (its `gh_config_dir`, when it names one),
`TMPDIR` and `CLAUDE_CODE_TMPDIR`, both `<data dir>/spaces/<space>/tmp` (created `0700`). Claude
Code gives sandboxed commands `TMPDIR=$CLAUDE_CODE_TMPDIR` (default `/tmp/claude`) whatever
`TMPDIR` says, so both are set. A checkout with no space (its root and its remote name different
jobs, or it is quarantined) is denied every isolated root that does not hold it, every space's
folder and gh profile and `.orch/`, and gets no environment.

In Claude Code settings this is `sandbox.enabled: true`, `sandbox.allowUnsandboxedCommands: false`,
each denied folder in `sandbox.filesystem.denyRead` and `denyWrite` and as `Read(//<path>/**)` and
`Edit(//<path>/**)` in `permissions.deny` (Read rules also cover Grep and Glob, Edit rules Write
and the other edit tools), the space's temp folder in `sandbox.filesystem.allowWrite`,
`enableAllProjectMcpServers: false` (a project `.mcp.json`, which the session's file tools can
write, would otherwise start servers outside the sandbox when every project server is enabled),
and the environment in `env`. A path holding a character Claude Code's rules read as a pattern
(`*?[]{}()!\`) gets no permission rule; the sandbox still denies it to shell commands.

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
  `sandbox` switch changes. A new clone gets its file within one interval plus one sync.
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
- Problems (a tracked or unmergeable file, a link, a placement error) are logged by orchd when
  they change.

**Launcher.** `orch run <space> -- claude|codex [args...]` starts a session with the space's
profile from any folder. It reads the applied policy through `DiffSpaces` (operator-only), so it
works in your own terminal and not inside an agent session. It refuses a space not in the policy,
and a folder another space holds or whose space cannot be decided unless `--force`; a folder no
root or remote places is allowed. It then replaces itself with the harness (`exec`), with the
variables set for the harness only:

- *Claude Code:* `CLAUDE_CONFIG_DIR=<data dir>/spaces/<space>/claude`. Its `settings.json` is your
  own (`$CLAUDE_CONFIG_DIR` or `~/.claude`) with the space's rules merged in, rewritten at every
  launch, so orch's hooks and your later changes carry over. `CLAUDE.md`, `agents`, `commands`,
  `skills` and `plugins` are links to yours, unless the profile has its own file there.
- *Codex:* `-c default_permissions="orch_<space>"` and `-c permissions.orch_<space>={extends=
  ":workspace",filesystem={"<path>"="deny",...}}` before your arguments, with your own
  `CODEX_HOME` and login. A checkout's `.codex/config.toml` cannot change it (SP-004). Codex has no
  per-checkout settings path, so the launcher is how a Codex session gets the denies.
- With the `sandbox` layer off, the session gets no deny rules but keeps the environment, and the
  launcher says so. When orchd cannot report the switch, the rules apply.

**Logging in to a per-space profile.** SP-004 measured that a fresh `CLAUDE_CONFIG_DIR` starts
logged out (`Not logged in · Please run /login`), so each space's Claude Code profile needs its own
login once: run `orch run <space> -- claude` and `/login` in it. Whether that login then persists
per profile was not measured in SP-004. The gh profile is separate: log in to it with
`GH_CONFIG_DIR=<gh_config_dir> gh auth login`; orch never handles either token.

**Limits.**
- Root paths of other jobs are written into every checkout's settings, so a session can read
  which folders are denied (not what they hold). Name job roots neutrally.
- Paths match by case in Claude Code's permission rules; macOS volumes are usually not case
  sensitive, so a tool given a differently cased path may pass the `Read` rule (the sandbox
  matches the real path).
- The shared `/tmp` stays writable by every space (D-53); only `TMPDIR` is per space.
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
  `spaces/` (`orch.db`, the command log) and the default gh login (`~/.config/gh`) stay readable
  to every space; self-protection (SP-042) covers orch's own files.
- Shell commands in a checkout cannot write its `.claude` folder, so a `git checkout` or `git pull`
  run by the session that changes tracked files under `.claude/` fails there; the file tools
  still edit them, and the user's own terminal is not sandboxed.
- Gemini is not covered (D-20).

## Status checks

- **Drift:** each adapter's pending edits. Items turned off in orch are not drift; the
  `claude-code` line reports `N disabled by you` (instruction modules included).
- **Instruction footprint:** for each harness whose entry file exists, follow `@` imports (depth 5,
  each file once; for Codex, the shared policy and each module path it lists) and report bytes and estimated tokens (`(bytes+3)/4`), the context cost of every
  session.
- **Repository instructions:** inside this repo, `AGENTS.md`, `CLAUDE.md` and the shared policy
  are compared with `HEAD`, and `CLAUDE.md` must be exactly `@AGENTS.md`.
