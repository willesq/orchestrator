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
brew install --cask willesq/tap/orch-app   # optional: the menu bar app
```

The cask needs macOS 15.2 or later, and installs `Orch.app`, a menu bar app showing orchd's
health with start and restart controls. It depends on the `orch` formula, which stays the owner
of the daemon.

## What `orch install` does

Install previews by default, backs up every file before changing it, leaves other tools' hooks
alone, and is safe to repeat. The default target is Claude Code; `--target codex`, `gemini` or
`all` wire the others. `--home` relocates everything for testing.

- **Hooks** run `orch-hook <event>` on session start and end, prompt submit, tool use,
  compaction and subagent stop. For Claude Code and Gemini each has a timeout of 5 to 15 s, so a
  stalled daemon cannot hold up a session. Codex hooks must be trusted in Codex before they run.
- **MCP servers** `rag` and `queue` are registered, and for Claude Code their tools are allowed
  (`mcp__rag__*`, `mcp__queue__*`) user-wide, so hooks and background agents can use them
  without a prompt. Your existing `deny` and `ask` rules win and are reported instead.
- **Agents and skills** from [`internal/install/assets/`](../internal/install/assets/) are
  installed for Claude Code (`~/.claude/agents/`, `~/.claude/skills/`). Each carries a
  `managed-by: orch` marker; install refuses to overwrite a file without one. Edit the repo
  copies: `orch status` reports hand-edited installed copies as drift.
- **launchd** keeps `orchd` running when Homebrew does not manage it, and an `orch` alias in
  `~/.zprofile` points at the installed binary.
- **Safety net** (Claude Code): see [below](#safety-net).
- **Settings** (Claude Code): `permissions.deny` gains `Read(~/.config/gh/**)`, `Read(~/.netrc)`
  and `Read(~/.docker/config.json)`, so the file tools cannot read credential files, and
  `attribution.commit` is set to `""`, so commits carry no coding-agent co-author trailer. Other
  attribution keys and every other setting are left alone. Install never adds `ask` rules.

Backups go to `~/.local/share/orch/backups`, owner-only.

## Safety net

For Claude Code, install also sets up [cc-safety-net](https://github.com/kenryu42/cc-safety-net),
a plugin that blocks destructive shell commands, pinned to the release this orch was reviewed
against (2.6.10), plus orch's own guard rules. There is nothing to do by hand:

1. A local marketplace, `orch-pinned`, is written to
   `~/.local/share/orch/marketplaces/orch-pinned/`. It serves the plugin at one commit, so an
   upstream release never reaches you until an orch release pins it. Pins are at least 14 days
   old.
2. `claude plugin` registers the marketplace and installs `cc-safety-net@orch-pinned`. An older
   copy from another marketplace (for example `safety-net@cc-marketplace`) is uninstalled
   first, and `cc-marketplace` is unregistered when no other installed plugin uses it. A pinned
   copy that is out of date is upgraded. A plugin you disabled stays disabled. After an install
   or upgrade, orch checks that Claude Code recorded the pinned commit and fails loudly if not.
3. The `orch-guards` rulebook is written to `~/.cc-safety-net/rules/orch-guards/rulebook.json`
   (or under `CC_SAFETY_NET_HOME`) and enabled in `rules/rule.json`, with `rtk` as a transparent
   wrapper so `rtk git push ...` is checked as `git push ...`. Your own rulebooks, overrides and
   wrappers are kept, and `rule.json` is backed up before it changes.

`orch-guards` denies only what the shared policy forbids, and never prompts:

| Blocks | Still allowed |
|---|---|
| `gh auth token`, `gh auth status --show-token`, `gh auth git-credential`, `security find-*-password -w/-g`, `security dump-keychain -d`, `git credential fill` | `gh auth status`; git's own `credential.helper=!gh auth git-credential` |
| `git push` to `main`/`master` (also `HEAD:main`, `refs/heads/main`), deleting them (`:main`), `--all`, `--mirror`, `--no-verify` | feature-branch pushes, including names like `feat/main-fix` |
| `git commit --no-verify`, `-n`, `--no-gpg-sign` | `git commit -S` |
| `gh repo delete`, `gh repo edit --visibility` | every other `gh` call, including `gh api` writes, PR comments, edits and merges |

orch owns `orch-guards` and overwrites it on upgrade; put personal rules in a rulebook of your
own and list it in `rule.json`. `orch status` shows a `safety-net` line, and Orch.app's Safety
Net tab shows the same state with Install, Upgrade and Repair rules buttons.

Limitations:

- Rules match exact tokens after cc-safety-net parses the command, so some forms get through:
  - `git -c commit.gpgsign=false commit` (a global `commit.gpgsign=true` still applies);
  - a push refspec naming another source branch, such as `git push origin feat:main`; branch
    protection on the server is the backstop;
  - a credential command assembled inside a Python or Node string.
- Rule files are not tamper-resistant: an agent can edit `~/.cc-safety-net/rules/`. They guard
  against mistakes, not a hostile agent. orch does not write cc-safety-net's `policy.json` and
  leaves its safety level at `standard`.
- A rulebook that fails validation is skipped silently by cc-safety-net; the Safety Net tab and
  `orch status` report it.
- Running Claude Code sessions load a newly installed plugin only after a restart.

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
make proto      # regenerate gRPC code (Go and Swift) after editing orchd.proto
make app        # build/app.noindex/Orch.app (needs Xcode 26+); make app-cover for its tests
make app-install  # build and install /Applications/Orch.app, replacing and relaunching it
```
