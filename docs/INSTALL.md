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

## GitHub accounts per space

One gh login is enough. With [spaces](OPERATIONS.md#spaces), every space uses whichever gh login
you already have (`gh auth login`) and your normal Claude Code configuration, with nothing to set
up (D-59). The trade-off: with one login the GitHub account is not a boundary between jobs. A
session working for one job can reach any repo that login can. The tool guard, memory spaces and
the folder denies still keep the jobs apart.

To make the account a boundary too, give a job (isolated space) its own GitHub account. Make a
directory under your home, outside every git repository, that only you can open, name it as the
space's `gh_config_dir`, and log in to it yourself:

```sh
mkdir -m 700 ~/.config/gh-acme
GH_CONFIG_DIR=~/.config/gh-acme gh auth login
```

orchd then runs every gh call about that space's orgs as that account: docs sync, PR monitoring,
the GitHub tab's scopes and repo picker, reviews, PR outcomes and org identity checks. It never
falls back to the default login for them. A call that account cannot read is refused with that
reason, not retried as another login. orchd also refuses the directory if it is not an absolute
path under your home, owned by you and closed to other users (`chmod 700`). Until the profile is
logged in and usable, the space's orgs are not synced, and `orch status` names the space and
what to fix. orch checks a profile only with `gh auth status`; it never reads the profile's files
or its token. See [internals/config.md](internals/config.md#spaces-policy).

A space can also name its own `claude_config_dir`, a Claude Code folder with its own login, used
by `orch run <space> -- claude` ([internals/install.md](internals/install.md#per-space-harness-settings)).
`orch status` and Orch.app's Settings → Spaces show which login each space uses and whether it is
logged in; orch reads only `gh auth status` and `claude auth status`.

Each isolated space's own account may make at most `github.isolated_account_hourly_budget` GitHub
API calls an hour (1000 by default; 0 is no cap). Past it, that account's docs sync and PR
watching wait for the next hour and its watched items stay as they were; reviews still run, and
other accounts are not affected.

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

## Turning settings off

The app's **Claude** tab (or `orch settings`) lists every hook, permission rule and plugin in
`~/.claude/settings.json` and every cc-safety-net rule, marked as added by orch, by you, or found
in the file. Turning one off takes effect at once (Claude Code and cc-safety-net reload their
files) and is remembered in this machine's `orch.db`:

- `orch install --apply` does not put back anything you turned off, and `orch status` does not
  call it drift; it reports `N disabled by you`. Items orch adds can be turned off but not
  removed.
- Turning an item back on restores exactly what was in the file.
- The tab also adds and removes hooks and permission rules.
- Edits made to the files by hand or by other tools are picked up within a few seconds; the file
  wins, so an entry another tool re-adds shows as on again, and one removed elsewhere is marked
  removed rather than put back.
- Choices are per machine. Nothing is synced: a rule off on one Mac stays on on another.
- If `orch.db` cannot be read, `orch install` stops rather than re-enabling what you turned off.

```sh
orch settings list                       # everything, with state and origin
orch settings disable hook 'SessionStart/orch-hook session-start'
orch settings enable rule orch-guards/gh-auth-token
```

Details: [internals/settings.md](internals/settings.md).

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
- A rulebook that fails validation is skipped silently by cc-safety-net; Settings → Safety Net and
  `orch status` report it.
- Running Claude Code sessions load a newly installed plugin only after a restart.

## Shared agent policy

[`internal/install/agent-instructions.md`](../internal/install/agent-instructions.md) is the
short always-on header of the shared policy. Everything else is an **instruction module** in
[`internal/install/modules/`](../internal/install/modules/), one file per intent: exploration,
testing, security, credentials, Git, commit signing, memory, PR review, and personal workflow
rules. Guidance for specific work (PR review, Go and Python) is in
[`internal/install/policy/`](../internal/install/policy/), read only when that work starts.

All of it is embedded in the CLI. Install writes the header to `~/.config/orch/AGENTS.md`, each
module to `~/.config/orch/modules/`, and the policy files to `~/.config/orch/policy/`, then points
each harness at them: Claude Code and Gemini import the header and each enabled module; Codex's
`AGENTS.md` (or a nonempty `AGENTS.override.md`) tells the agent to read them. Personal
instructions stay in each harness's own entry file.

**Turning modules on and off.** Every module is on by default. Turn one off for this machine in
Orch.app (Settings → Claude, **Instructions**) or with `orch settings disable module
<name>`; for example, turn off `gpg-signing` on a machine that does not sign commits. The choice
lives in this machine's `orch.db`. orchd removes that module's import from each managed block
within seconds and puts it back if the block is edited by hand; the next session loads the
change. The app shows each module's full text.

**Subagents and skills.** The Claude Code subagents and skills orch ships (its own, plus the
author's `adversarial-qa`, `adversarial-reviewer`, `security-audit` and other agents, and the
`adversarial-qa`, `api-design` and `qa-fixer` skills) are toggled the same way: Settings → Claude,
**Subagents** or **Skills**, or `orch settings disable agent <name>` / `disable skill <name>`.
Off removes the files orch installed for it from `~/.claude` (backed up first); on puts them
back. A copy of your own that is identical to the shipped one is taken over; one you changed is
left alone and reported. To ship another one, add its files under
`internal/install/assets/claude/agents/` or `skills/<name>/` with the `managed-by: orch` marker,
and rebuild.

**Adding a module.** Add a Markdown file to `internal/install/modules/` with a frontmatter
header, then rebuild:

```markdown
---
title: "Commit signing"
group: Git & GitHub
order: 62
description: "Sign commits with git commit -S."
---

## Commit signing

- Sign commits with `git commit -S` through the configured signing integration.
```

The file name (lowercase letters, digits and dashes) is the module's ID. `order` places its
import; `default: false` ships it off; `sensitive: true` marks part of the agents' security
policy (`security`, `credential-rules`, `security-audit-agent`): turning one off asks for
confirmation in the app, needs `--yes` with `orch settings disable`, and `orch status` names it
while it is off. The header is stripped from the installed copy, so agents
read only the body.

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
