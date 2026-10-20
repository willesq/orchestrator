# Installing orch

## Homebrew

The repo and tap are private: tap over SSH, and have `gh` authenticated with read access to
`willesq/orchestrator`; the formula uses it to download the release.

```sh
brew install gh && gh auth login
brew tap willesq/tap git@github.com:willesq/homebrew-tap.git
brew install willesq/tap/orch     # binaries, plus gh and onnxruntime
orch model fetch                  # embedding model, ~90 MB
orch install                      # preview; then orch install --apply
brew services start orch          # start orchd
orch check                        # verify preconditions
```

## What `orch install` does

Install previews by default, backs up every file before changing it, leaves other tools' hooks
alone, and is safe to repeat. The default target is Claude Code; `--target codex`, `gemini` or
`all` wire the others. `--home` relocates everything for testing.

- **Hooks** run `orch-hook <event>` on session start and end, prompt submit, tool use,
  compaction and subagent stop. Codex hooks must be trusted in Codex before they run.
- **MCP servers** `rag` and `queue` are registered, and for Claude Code their tools are allowed
  (`mcp__rag__*`, `mcp__queue__*`) user-wide, so hooks and background agents can use them
  without a prompt. Your existing `deny` and `ask` rules win and are reported instead.
- **Agents and skills** from [`internal/install/assets/`](../internal/install/assets/) are
  installed for Claude Code (`~/.claude/agents/`, `~/.claude/skills/`). Each carries a
  `managed-by: orch` marker; install refuses to overwrite a file without one. Edit the repo
  copies: `orch status` reports hand-edited installed copies as drift.
- **launchd** keeps `orchd` running when Homebrew does not manage it, and an `orch` alias in
  `~/.zprofile` points at the installed binary.

Backups go to `~/.local/share/orch/backups`, owner-only.

## Shared agent policy

[`internal/install/agent-instructions.md`](../internal/install/agent-instructions.md) holds the
agent-neutral coding expectations: exploration, testing, security, credentials, Git, docs and
memory. Guidance for specific work (PR review, Go and Python) is in
[`internal/install/policy/`](../internal/install/policy/), read only when that work starts.

Both are embedded in the CLI. Install writes them to `~/.config/orch/AGENTS.md` and
`~/.config/orch/policy/`, and points each harness at them: Claude Code and Gemini import the
file; Codex's `AGENTS.md` (or a nonempty `AGENTS.override.md`) tells the agent to read it.
Personal instructions stay in each harness's own entry file.

To change the policy, edit the repo source, then upgrade or rebuild and rerun install.
`orch status` reports when the installed copy differs from the one the CLI ships, and, inside
this repo, when `AGENTS.md`, `CLAUDE.md` or the policy differ from the last commit.

## Build from source

The embedder uses cgo: it links `libtokenizers.a` (from a
[daulet/tokenizers](https://github.com/daulet/tokenizers) release) and loads ONNX Runtime at
run time.

```sh
brew install onnxruntime
# place libtokenizers.a in build/lib/darwin-arm64/
make build      # build/bin/{orch,orchd,orch-hook}
make test       # pure-Go tests, no native libraries needed
make cover      # per-file coverage gate (>= 80%)
make proto      # regenerate gRPC code after editing orchd.proto
```
