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
| `~/.claude/settings.json` | hooks, MCP servers, permissions |
| `~/.claude.json` | MCP servers, only if the file exists |
| `~/.claude/CLAUDE.md` | managed block importing the shared policy |
| `~/.claude/agents/*.md`, `skills/*/SKILL.md` | shipped agents and skills |
| `~/.zprofile`, LaunchAgent | see [common](#common) |

**Hooks:** for each event, `hooks.<Event>` gets
`{"hooks": [{"type": "command", "command": "<bin>/orch-hook <slug>"}]}`. Events: `SessionStart`,
`UserPromptSubmit`, `Stop`, `SessionEnd`, `PreCompact`, `PostToolUse`, `SubagentStop`, mapped to
the slugs in [hooks.md](hooks.md). An entry is ours if its command contains the bin path,
`orch-hook`, or `orchestrator/hooks/`; ours is updated in place, duplicates removed, and other
tools' entries kept in order.

**MCP servers:** `mcpServers.rag = {"command": "<orch>", "args": ["mcp", "rag"]}`, likewise `queue`.

**Permissions:** for each server, append `mcp__<server>__*` to `permissions.allow` so hooks and
background agents, which cannot answer prompts, can use memory. Skip it if a `deny` or `ask` rule
covers the server (`mcp__*`, `mcp__<s>`, or any `mcp__<s>__…` rule), and report why; skip if an
allow rule already covers it. A `permissions` value of an unexpected JSON type aborts the install.

**Agents and skills:** agents `dev`, `dispatcher`, `ops`, `research`, `reviewer`, `triage`;
skills `orch-dispatch`, `orch-pr-status`, `orch-rag-ingest`, `orch-rag-query`,
`orch-repo-onboard`, `orch-task-status`, embedded from `internal/install/assets/`. Each carries a
`managed-by: orch` marker: missing files are created, marked files updated, symlinks into the
retired `~/.claude/orchestrator` checkout replaced; any other file aborts the install rather than
being overwritten.

## Codex

- `~/.codex/config.toml`: `[mcp_servers.orch_rag]` and `[mcp_servers.orch_queue]` with the same
  command and args.
- `~/.codex/hooks.json`: same hook shape and events as Claude Code. Codex runs hooks only after
  the user reviews and trusts them.
- `~/.codex/AGENTS.md` (or a nonempty `AGENTS.override.md`, which Codex prefers): a managed block
  telling the agent to read the shared policy, since Codex does not expand `@` imports.

## Gemini

`~/.gemini/settings.json`: the same MCP servers, and hooks mapped `SessionStart`, `BeforeAgent`
(prompt), `SessionEnd`, `AfterTool`. `~/.gemini/GEMINI.md` imports the shared policy. Gemini's
payload schema is assumed to match Claude Code's; this is not verified.

## Common

- **Shared policy.** `internal/install/agent-instructions.md` is written to
  `~/.config/orch/AGENTS.md`, and each `internal/install/policy/*.md` to `~/.config/orch/policy/`.
  orch owns these files. Harness entry files get a block between `<!-- BEGIN orchestrator-managed -->`
  and `<!-- END orchestrator-managed -->` containing `@<path>` (Claude, Gemini) or a read directive
  (Codex). Only the block is ever replaced; mismatched markers abort.
- **Shell alias.** `~/.zprofile` gets a marked block `alias orch="<bin>/orch"`.
- **LaunchAgent.** `~/Library/LaunchAgents/com.orchestrator.orchd.plist` (`RunAtLoad`,
  `KeepAlive`, logs under `~/Library/Logs/orchestrator/`), skipped when orchd is installed under
  Homebrew's `Cellar`, since `brew services` supervises it.

## Status checks

- **Drift:** each adapter's pending edits.
- **Instruction footprint:** for each harness whose entry file exists, follow `@` imports (depth 5,
  each file once) and report bytes and estimated tokens (`(bytes+3)/4`), the context cost of every
  session.
- **Repository instructions:** inside this repo, `AGENTS.md`, `CLAUDE.md` and the shared policy
  are compared with `HEAD`, and `CLAUDE.md` must be exactly `@AGENTS.md`.
