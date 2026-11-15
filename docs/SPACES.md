# Memory spaces

Memory spaces keep work for different jobs apart on one Mac. Content from one employer never
reaches a session working for another, while open-source docs stay readable everywhere. orchd
enforces this in code, not in prompts.

Spaces are **off by default**. Until you turn them on, orch works as it did before spaces, and
nothing on this page applies. If you use orch for one job, or don't mix jobs on this Mac, you can
leave them off.

## Concepts

- **A space** is the unit of separation. Each space has a kind and owns its **bindings**: GitHub
  orgs, local folder roots, web URL prefixes, chat tenants (`slack:T0123`, `teams:<tenant>`), an
  outbound terms file, and optionally its own gh and Claude Code logins.
- **Kinds.**
  - `public`: readable from every space. It accepts only content verified public, or under a root
    whose `public_paths` you declared public. Sessions and agent notes never go there.
  - `isolated`: one per job. It reads itself, public, and any private space it lists. It never
    reads another isolated space; a policy that tries is rejected.
  - `private`: personal. Like isolated, and it may also read the other private spaces it lists.
- **Placement.** orchd decides where every chunk and record goes when it is written. The deepest
  bound root holding a path decides its space, and the org that owns a GitHub repo decides the
  repo's (fork parents are ignored). When a folder and its git remote disagree, the stricter one
  wins (public < private < isolated). Two isolated spaces disagreeing is a conflict: nothing is
  indexed. A nested root may only be stricter than every root above it.
- **Quarantine.** Content orchd cannot place goes to quarantine, which no session reads. It never
  defaults to public.
- **Session pins.** A session belongs to the space of its first directory for its whole life,
  including resume, compact and subagents. orchd finds a caller's session from the process tree
  the kernel reports, not from anything the caller sends (on macOS; see
  [internals/daemon.md](internals/daemon.md) for the limits).
- **Policy revisions.** The policy lives in `orch.db` as numbered revisions. Anyone, agents
  included, may save a draft; only the operator applies one, naming the revision they reviewed.

Each space has its own Qdrant collection, plus one for public and one for quarantine. A search
reads only the collections its caller may read.

## Turning spaces on and off

Turn them on or off in Orch.app (Settings → Spaces) or from a terminal outside any agent session:

```sh
orch spaces on                        # move memory back into its spaces, then enforce the policy
orch spaces off --confirm             # stop enforcing; the policy and its records are kept
orch install                          # then add (on) or remove (off) the tool hooks
```

The switch is the setting `spaces.enabled`. While a policy is applied, only Orch.app can change
it. An install that applied a policy before the switch existed comes up with spaces on, so
upgrading changes nothing it enforces.

While spaces are off:

- harnesses get no tool hooks before every tool call;
- orchd keeps no command log and no session pins;
- the guard, outbound terms and sandbox rules are inactive;
- one memory store serves every session.

**Turning spaces off** stops every layer at once. orchd then moves every space's memory back into
one store in the background, and `orch status` shows the move. **Quarantined content becomes
readable again**, as it was before spaces; it keeps its quarantine label.

**Turning spaces on** moves memory back into each space's collection first. Nothing is enforced
until that move finishes, and `orch status` shows `switch: turning on`. orchd then enforces the
applied policy and relabels anything written while spaces were off. Restart sessions that were
running, so they get a pin.

While spaces are off, `orch spaces apply`, `enforce`, `sandbox`, rule changes, `retag`,
quarantine purges, `orch rag relabel`, `orch run` and `orch guard git-hook --install` are
refused. Reading commands still answer from the kept records.

Harness files change only at the next `orch install`; until then `orch status` reports them as
drifted.

## Setting up a policy

**In Orch.app.** With spaces on, the Spaces pane offers **Set Up Spaces…**. It scans your usual
checkout folders and gh logins, proposes one space per GitHub owner (Job: keep separate,
Personal, Open, or Leave out) with folder roots, and saves a draft for you to review and apply.
The pane's **Edit** page covers everything else: bindings, reads, public paths, terms files,
outbound rules, a "where would this go?" tester, per-harness coverage and retagging sources. See
[internals/app.md](internals/app.md#spaces).

**From a terminal.** Save a policy as JSON, review it, then apply it:

```json
{
  "spaces": [
    {"name": "oss", "kind": "public", "github_orgs": ["oss-org"],
     "roots": ["~/code"], "public_paths": ["~/code/oss-org/**"]},
    {"name": "personal", "kind": "private", "github_orgs": ["my-login"],
     "roots": ["~/code/mine"]},
    {"name": "acme", "kind": "isolated", "github_orgs": ["acme"],
     "roots": ["~/work/acme"], "chat": ["slack:T0ACME"],
     "terms_file": "~/.config/orch-terms/acme.txt",
     "gh_config_dir": "~/.config/gh-acme"}
  ]
}
```

```sh
orch spaces draft < policy.json       # save a draft revision; changes nothing
orch spaces diff [--full]             # what the newest draft changes, and its problems
orch spaces apply --revision N        # enforce draft N
```

Apply takes only the newest draft, the one you reviewed, and only when it validates. An agent
session may save a draft; the diff says so and shows every value it changes, and applying it
needs `--accept-agent-draft`. A saved draft, or an edit to `orch.db`, changes nothing that is
enforced; `orch status` shows it in its `spaces` section as drift. The first apply can run from
your terminal. After that, apply is Orch.app's alone (see [who is the operator](#who-is-the-operator)).
The schema and its limits are in [internals/config.md](internals/config.md#spaces-policy).

Apply looks up every bound GitHub org with `gh` and records its node ID, so it needs GitHub to be
reachable. If an org's login is later deleted and registered by someone else, the next docs sync
sees a different node ID. orchd then stops syncing that org, `orch status` reports an unconfirmed
org, and `orch spaces diff` names it. Check the org on GitHub. If the change is expected, apply the
newest revision again with `--confirm-github-org ORG`. Until you do, any apply is refused.

Local folders under a public root (such as `~/code`) are public only when you list them in that
public space's `public_paths`: `~/code/x` for the files directly in it, `~/code/*/docs` for one
folder name, `~/code/x/**` for everything below. There is no `!`; list only what is public.
Everything else under the public root is quarantined, whatever its `.git` says.

`orch spaces why <path|url|owner/repo|slack:ID|teams:ID>` shows how the applied policy places a
target: every bound root holding it from shallowest to deepest, what its GitHub remote
contributes, the rule that decided and the result. For a path it also says where a session
started there would write and which space it would read as. Inside an agent session orchd answers
only when every space in the answer is public. See [internals/memory.md](internals/memory.md#placement).

After applying a policy, and after each later revision, relabel what was stored before it (the
Spaces pane's **Relabel Now**). See [MEMORY.md](MEMORY.md#relabelling-after-a-spaces-policy-change).

## Who is the operator

Some actions are the operator's: applying a policy, the enforcement switches, relabel, retag,
reading every space, and the sensitive settings. Agent sessions and queued tasks are never the
operator.

- **No policy applied:** your own terminal, outside any agent session, is the operator, as before
  spaces.
- **A policy applied:** only **Orch.app** is the operator (D-71). orchd recognizes it by its code
  signature, so reinstall it (`make app-install`) if it was built before ad-hoc builds got the
  hardened runtime. A terminal reads public memory only, and its operator commands are refused.

To run operator commands from a terminal under a policy, unlock it:

```sh
orch spaces unlock --for 15m [-- <command>]   # ask Orch.app for operator scope (at most 60m)
orch spaces lock                              # end the unlocks covering this terminal
```

`unlock` prints its process and terminal (e.g. ttys003) and asks Orch.app, whose menu bar panel
(and a notification) shows the request with its command, process and terminal. Confirm it there
with Touch ID or your password only when the terminal is yours (Terminal can show its tty name in
the window title), or deny it. Orch.app then shows a code: type it into the terminal that asked.
Nothing is granted until the code matches, so a request an agent filed at the same time gets
nothing even if you confirm it by mistake; if no terminal of yours waits for a code, cancel it in
the panel.

Once granted, orch starts your shell (or the command after `--`, e.g. `orch spaces unlock --for
30m -- orch rag relabel`, which is safer) as the operator until the time is up, the shell exits,
or `orch spaces lock` (or Lock in the panel) ends it. Only that shell and what it starts are the
operator: they read every space and run operator tasks such as relabel, retag and diff. An agent
started in it is not the operator, and its first call to orchd ends the unlock. The unlock is
refused inside an agent session or a queued task, and every unlock ends when orchd restarts.

Changes that weaken protection stay **Orch.app's alone, even under an unlock** (D-78), because
they outlast the unlock:

- applying a policy;
- switching an enforcement layer off or to a looser state;
- the sandbox setting;
- outbound rules and a space's terms;
- the settings orchd keeps for the operator under a policy: sensitive config keys (the spaces
  switch included), harness and cc-safety-net settings, restart, PR landings and purging docs.

Direct-message search is meant to be Orch.app's alone too ([chat](#chat-feeds-and-direct-messages)). See
[internals/daemon.md](internals/daemon.md#operator-evidence) and
[internals/daemon.md](internals/daemon.md#operator-unlock).

## What is enforced

Each layer has an operator-only switch, optionally timed:

| Layer | What it does | Switch states |
|---|---|---|
| `memory` | Every read path returns only the spaces the caller may read | on, off |
| `guard` | The pre-tool hook refuses tool calls that reach another job's folders, repos or orgs | `block-other-jobs` (default), `strict`, `log`, `off` |
| `terms` | Refuses a commit, push or GitHub write carrying one space's terms outside it | on, off |
| `sandbox` | Generated OS sandbox and file-tool deny rules in each checkout | on, off |
| `hooks` | Claude Code's pre-tool-use and cwd-changed hooks | on, off |

```sh
orch spaces enforce                       # every layer's state
orch spaces enforce guard off --for 15m   # switch a layer, optionally for a time
```

`--for` makes the switch revert by itself. Every change is logged with its time and caller. While
a layer is off, `orch status`, Orch.app's banner and every new session's start digest say so, and
the command log keeps recording. orch settings and `orch install` cannot turn the tool hooks off;
`orch spaces enforce hooks off` can. Under a policy, tightening a layer needs an unlocked terminal
or Orch.app, and loosening one needs Orch.app. See
[internals/daemon.md](internals/daemon.md#enforcement-switches).

### Command log

orchd keeps a command log of every tool call and directory change Claude Code and Codex sessions
report, in `~/.local/share/orch/toollog.db` (owner-only, never indexed into memory). Commands are
stored credential-redacted and capped at 2 KB.

```sh
orch spaces log [<session>|task:ID]       # a session's command log, in order
orch spaces log summary [--since 168h]    # per space: tool calls, directory moves, moves into another space
orch spaces log verify <session|task:ID>  # check a session's command log was not changed
orch spaces log retention [3d|7d|30d]     # show or set how long ended sessions' logs are kept
```

Inside an agent session, `log` prints only that session's own records; `summary` and `verify`
are operator-only. A session's records are deleted 7 days after it ends and its process has
exited (3, 7 or 30 days); refused calls are kept 90 days. A running session is never deleted,
however long it sits idle, only cut to its newest 50 MB, and past 500 MB in total the sessions
that ended first go early, all but their refused calls. `verify` shows how far retention cut a
session. See [internals/daemon.md](internals/daemon.md#command-log).

Each call the guard or the outbound terms check refuses is also appended to
`~/.local/share/orch/events/spaces.jsonl` (owner-only) as one `refusal` event naming the session,
its space, the tool and the rule, and for a term its space, terms-file line and hash; never the
command or the term. Orch.app posts a notification for each one made while it runs. See
[internals/hooks.md](internals/hooks.md#refusal-events).

### Outbound terms

A space's `terms_file` lists words that must not leave it: client names, project code names.
The check refuses a commit, push or GitHub write that would carry them to a destination outside
the space. The file must live outside every git tree. Terms never appear in logs, refusals, `orch`
output or the app; `orch spaces terms list` shows each file's count and fingerprint only, and
`orch spaces terms set <space> < terms.txt` replaces a space's terms from input (Orch.app only
under a policy).

How strictly the check reads commands is a baseline of named rules, all on by default:

| Rule | On (default) |
|---|---|
| `fail_closed_unreadable` | a command the check cannot read in full is refused |
| `gh_default_repo` | a gh write without `-R` counts only the repo gh would use |
| `plain_variables` | `$PWD`, `$USER` and `$HOME` are read |
| `command_assignments` | variables set earlier in the same command are read |
| `git_config_harmless_keys` | `git -c` with harmless keys is read |
| `pending_work_after_staging` | after `git add` in the same call, a commit or push also checks the work tree and every branch |

**Relax** it by switching a baseline rule off (`orch spaces terms rules disable <id>`, or its
switch in Orch.app → Settings → Spaces → Outbound rules). Switching off `fail_closed_unreadable`
lets what cannot be read through; switching off any other rule reads commands more strictly than
the default or checks less (`pending_work_after_staging`). **Tighten** it by adding rules of your
own: `command <program>` (check a program such as `glab` like a git or gh write), `destination
<owner[/repo]>` (no space's terms may reach it, e.g. a public repo of a job's org) and
`fail_closed <text|re:regex>` (refuse any tool call whose tool name or text matches, e.g. a GitHub
MCP tool). An added rule can only make the check stricter; one that would allow or exempt is
rejected. **Switch the whole check off** with the terms layer: `orch spaces enforce terms off
[--for 15m]`.

```sh
orch spaces terms rules                              # the rules and their state
orch spaces terms rules disable plain_variables      # switch a baseline rule off
orch spaces terms rules add fail_closed < rule.txt   # add a stricter rule; the value is read from input
```

Each change is logged with its time and caller; under a policy, rule changes are Orch.app's
alone. `add` reads the value from its input, so it stays out of your shell history. A rule's value,
like a term, is never shown: rules are named by id (`user-1`, …), so keep your own note of what
each one holds. A refusal by one of your rules names it: `the operator's outbound rule user-1
refuses it`. If `orch.db`'s rules cannot be read, `orch spaces terms rules` warns, and every shell
command and MCP tool is refused while terms apply, until you make any change. See
[internals/hooks.md](internals/hooks.md#outbound-rules).

**Git hooks.** The terms check in the tool hook reads only what an agent's command says. `orch
guard git-hook --install --apply`, run in a repository (or with `--repo` or `--hooks-dir` for a
shared `core.hooksPath`), adds `commit-msg` and `pre-push` hooks that run the same check on every
commit and push there, including your own and those scripts make. A hook already there keeps
running after orch's check passes; `--uninstall --apply` puts it back, and `orch uninstall`
removes every hook orch recorded. Without `--apply` both only show what they would change.
`--no-verify` skips the hooks. See
[internals/install.md](internals/install.md#git-hooks-for-outbound-terms).

### Per-space harness settings

While a policy is applied and the sandbox setting writes rules for a checkout, orchd keeps
`.claude/settings.local.json` in every git checkout under a space's roots: the OS sandbox on,
and other jobs' folders denied to shell commands and to Claude Code's file tools, plus its own
`TMPDIR` and, when the space names a `gh_config_dir`, its `GH_CONFIG_DIR` (otherwise gh uses your
default login). gh works in the sandboxed shell: a space on your default login may read that
login's folder inside the sandbox, a space naming its own profile reads only that profile, and gh
may reach `github.com`; the guard still refuses anything that names gh's files. Claude Code's
sandboxed commands get the space's `TMPDIR` only in sessions started with `orch run`; sessions
started in a checkout share Claude Code's `/tmp/claude-<uid>` (D-58). Your own entries in that
file stay; orch adds the file to the checkout's `.git/info/exclude` when nothing ignores it, and
never writes a tracked one. A new clone gets its file within a minute.

The sandbox setting is your lasting choice, `orch spaces sandbox auto|on|off` (Orch.app only
under a policy; each change is logged with its time and caller). `auto`, the default, writes the
rules only for sessions that something must be denied to: another isolated space's folders,
another space's own `gh_config_dir` or `claude_config_dir`, or (for a space naming its own
`gh_config_dir`) your default gh login's folder. So one job alone on your default logins gets
none. `on` writes them for every space while a policy is applied; `off` writes none (a space's
own `GH_CONFIG_DIR` stays). `orch status` and Orch.app show it. It does not make session digests
warn that enforcement is off, but each session's next start digest names it once after you change
it, and after orchd starts while it is off. `orch spaces enforce sandbox off [--for 15m]` is the
debugging switch: it takes everything out, and every digest says so while it is off.

`orch run <space> -- claude|codex` gives a session the same rules from any folder. Codex has no
per-checkout settings, so this is how Codex sessions get them. By default it uses your own logins
(D-59): your normal Claude Code configuration, with the rules added through `--settings`, and
your default gh login. Only a space that names its own `claude_config_dir` runs Claude Code with
that folder as `CLAUDE_CONFIG_DIR`, which needs its own `/login` the first time; only one that
names a `gh_config_dir` gets that `GH_CONFIG_DIR`. Run it in a folder of that space or in one no
space holds; `--force` allows another space's folder. `orch run` reads the applied policy as the
operator, so under a policy it works only from an unlocked terminal, e.g. `orch spaces unlock
--for 15m -- orch run acme -- claude` (RC-1901). See
[internals/install.md](internals/install.md#per-space-harness-settings).

### Sessions

`orch sessions list [--all]` shows each pinned session's space, directory and state. orchd reads
liveness from the kernel: a session left open without `/exit` stays `idle` for as long as its
claude or codex process runs, and is marked `ended` within a minute of the process exiting, even
without SessionEnd. `claude --resume` in a new process keeps the session's pin and space. Running
claude, codex or gemini processes with no pin (started before the hooks were installed) show as
`unpinned`. See [internals/daemon.md](internals/daemon.md#session-liveness).

## GitHub and Claude Code logins per space

Every space uses your default gh login and your normal Claude Code configuration unless it names
its own `gh_config_dir` or `claude_config_dir` (D-59). One login needs no setup, but then the
GitHub account is not a boundary between jobs. For a separate account, set the space's
`gh_config_dir` and log in to it yourself; orch never handles the token. Each such account has an
hourly GitHub API budget (`github.isolated_account_hourly_budget`). `orch status` and Orch.app's
Settings → Spaces show which login each space uses and whether it is logged in. See
[INSTALL.md](INSTALL.md#github-accounts-per-space).

## Memory in spaces

- **Relabel** after applying a policy, so earlier content reaches the spaces it belongs to.
- **Retag** moves one source to another space and keeps it there:
  `orch spaces retag <source_uri> <space> [--confirm]`.
- **Quarantine** holds what could not be placed: `orch quarantine list`, `assign`, `purge`.

All three are operator actions and re-embed nothing. See
[MEMORY.md](MEMORY.md#relabelling-after-a-spaces-policy-change).

## Chat feeds and direct messages

orch has connectors for Slack and Microsoft Teams (`internal/feeds`), which store allowlisted
channels and, per tenant, direct messages (the `feeds.chat.*` settings). A chat tenant's binding
decides the space, and an unbound tenant goes to quarantine. **Nothing runs the connectors yet:**
orchd has no schedule for them and no setting for each tenant's login (RC-1650, RC-1701), so no
chat content is stored today. Neither connector has been run against a live workspace.

Direct messages are never added to prompts or given to tasks. Searching them is the operator's
alone: Orch.app's Memory → **Direct messages** tab, or `orch search --direct-messages` from your
terminal while no policy is applied. Agent sessions and queued tasks are refused. Under a policy
the decision is that only Orch.app searches them (D-81), but orchd still lets an unlocked
terminal search them too (RC-1900). See [internals/memory.md](internals/memory.md#chat-feeds).

## Harness coverage

Spaces cover Claude Code and Codex. Gemini CLI gets orch's memory hooks but is not covered by
spaces (D-20).

| | Claude Code | Codex | Gemini CLI |
|---|---|---|---|
| Memory hooks (prompt context, session capture) | yes | yes | yes |
| Session pin and memory read by its space | yes | yes | no: reads like a terminal (public only under a policy) |
| Tool guard and outbound terms before each tool call | yes | yes, once you trust the hooks in Codex | no |
| Command log of tool calls | yes | yes | no |
| Directory changes logged (`CwdChanged`) | yes | partly: no such event; each call's working directory is recorded | no |
| Per-checkout sandbox and file-tool denies | yes | no; use `orch run <space> -- codex` | no |
| `orch run <space>` launcher | yes | yes | no |
| Tool hooks switch (`enforce hooks off`) removes them | yes | no (Codex's `hooks.json` is not managed by orch settings) | n/a |

While spaces are on, `orch status` prints a `<harness> tool hooks` line per harness under harness
wiring (`wired`, `missing <events>`, or `not covered by spaces` for Gemini), and the Spaces pane's
Edit → Coverage page shows the same per harness with live pinned and unpinned processes. See
[internals/install.md](internals/install.md#tool-hook-coverage).

## Limits

Spaces raise the bar against a confused or over-eager agent. They do not stop everything:

- **Files on disk.** Memory is enforced fully, but files are not. The tool guard reads tool calls'
  targets best-effort: a command it cannot read in full, or a path built at run time, can still
  reach another job's folder unless `strict` is on. The generated sandbox rules and a separate
  GitHub account per job are the hard boundary, and they cover Claude Code in checkouts and
  sessions started with `orch run`. Claude Code sessions started in a checkout share
  `/tmp/claude-<uid>` (D-58).
- **Hostile local processes.** Anything running as you can call orchd's socket, start a process
  that looks like a new session, read Qdrant's data directory or `orch.db`, or write orch's files.
  Pins, lineage checks and the Orch.app signature check stop a confused agent, not a determined
  attacker with your user account.
- **Model providers.** Whatever a session reads, including memory orch adds to its prompt, goes
  to that harness's model provider under your account's terms with them. Spaces decide what a
  session sees, not where the session sends it.
- **Paraphrase.** Outbound terms match words. They are a tripwire, not a classifier: "a large
  Seattle retailer" passes a terms file that lists the retailer's name.

Also:

- **Chat permissions.** If you can read a Slack channel, a connector can too; orch does not
  re-check the chat system's permissions after storing it.
- **Harnesses.** Gemini CLI and any harness orch does not install into are not covered (see
  [harness coverage](#harness-coverage)).

## Command reference

```sh
orch spaces on | off --confirm        # the master switch
orch spaces draft < policy.json       # save a draft revision
orch spaces diff [--full]             # review the newest draft
orch spaces apply --revision N [--accept-agent-draft] [--confirm-github-org ORG]
orch spaces list                      # the applied spaces: bindings, reads, terms file, logins (operator)
orch spaces scan ~/code [--offline]   # checkouts and gh orgs found, and where each goes (operator)
orch spaces why <target>              # which space a target's content goes to, and why (alias: explain)
orch spaces enforce [<layer> <state> [--for 15m]]
orch spaces sandbox [auto|on|off]
orch spaces terms list | set <space> < terms.txt | rules [disable|add ...]
orch spaces log [<session>] | summary | verify <session> | retention [3d|7d|30d]
orch spaces replay <session>          # decide its recorded tool calls again; list the verdicts that change (operator)
orch spaces retag <source_uri> <space> [--confirm]
orch spaces unlock --for 15m [-- <command>] | lock
orch quarantine list | assign <source_uri> <space> --confirm | purge ...
orch rag relabel [--dry-run]
orch guard git-hook --install|--uninstall [--apply]
orch sessions list [--all]
orch run <space> -- claude|codex [args...]
```

Every Spaces pane action has a CLI equivalent; the table is in
`internal/cli/testdata/spaces_pane_parity.json`. Implementation details are in
[internals/](internals/README.md); the plan that built spaces, with its decisions (D-ids) and
open follow-ups (RC-ids), is in [spaces/](spaces/spaces-features.json).
