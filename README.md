# orch — Claude Orchestrator (Go)

A local-first orchestration layer for agentic coding CLIs (Claude Code, Codex, Gemini).
One supervised daemon (`orchd`) owns a warm embedding model, a native Docker-free Qdrant,
a durable workflow engine, a source-freshness scheduler, and a PR babysitter. The CLI,
hooks, and MCP servers are thin clients that dial `orchd` over a Unix-domain socket.

macOS, single-user. Ships three binaries: `orch` (CLI), `orchd` (daemon), `orch-hook`
(harness lifecycle hooks).

## Install (Homebrew)

The tap and this repo are private, so tap over **SSH** (HTTPS tapping fails to
authenticate). The formula then authenticates the asset download with the gh CLI,
so an authenticated `gh` (with read access to `willesq/orchestrator`) is all you
need — no manual token export:

```sh
brew install gh && gh auth login   # once, if you haven't already
brew tap willesq/tap git@github.com:willesq/homebrew-tap.git   # SSH — the tap is private
brew install willesq/tap/orch     # pulls the prebuilt binaries + gh + onnxruntime
orch model fetch                  # download the embedding model (~90MB, once)
orch install --apply              # wire hooks + MCP + alias + launchd (backs up first)
brew services start orch          # start orchd (+ native qdrant, workflows, scheduler)
orch check                        # verify preconditions are green
```

`orch install` is **dry-run by default** — run `orch install` (no `--apply`) to see exactly
what it would change. It is surgical (preserves other tools' hooks) and idempotent.

For Claude Code, install registers the `rag` and `queue` MCP servers in the user-global
`~/.claude/settings.json` (and in `~/.claude.json` when present) and allows their tools in
`permissions.allow` as `mcp__rag__*` and `mcp__queue__*`. Registration alone is not enough:
under the default `ask` permission mode every call waits for interactive confirmation, so
lifecycle hooks and background agents — which have nobody to answer the prompt — lose access
to durable memory. Because the grant is user-global, it applies in every project, and the
dry-run summary says so before you apply it.

Existing rules are scanned before anything is appended, so repeat installs add no duplicates;
`mcp__*`, `mcp__rag` and `mcp__rag__*` all count as already covering a server. Both categories
that outrank `allow` hold the matching rule back entirely and are reported instead: a `deny`
rule, and an `ask` rule — the one you add precisely to keep a destructive tool such as
`rag_delete_source` behind a confirmation. Either is honored whether written as a blanket
`mcp__*`, a bare `mcp__rag`, a wildcard `mcp__rag__*`, or a single tool
`mcp__rag__rag_delete_source`. A `permissions` block, or an `allow`, `deny` or `ask` of an
unexpected JSON type, aborts the install with an error rather than being overwritten, and no
file is written when any edit fails to compute. Codex and Gemini use different permission
models and are unaffected.

To wire every supported coding agent, use `orch install --target all`, then
`orch install --target all --apply`. The default target remains `claude-code`;
`--target codex` and `--target gemini` install those harnesses individually.
There is no separate setup command: `install` handles harness configuration.

Install also ships the orchestrator's Claude Code subagents and skills, embedded in the
CLI from [`internal/install/assets/`](internal/install/assets/): `dev`, `dispatcher`, `ops`,
`research`, `reviewer`, `triage` into `~/.claude/agents/`, and the `orch-*` skills into
`~/.claude/skills/`. Claude Code target only; Codex and Gemini do not receive them yet.
The agents invoke tools such as `gittree` by name from PATH; orch does not ship them, so a
missing one fails with the shell's usual "command not found".

Each shipped file carries a `managed-by: orch` marker. Install creates missing files,
refreshes marked ones, and replaces symlinks left pointing into the retired
`~/.claude/orchestrator` checkout (removing the link, never its target). Any other file
at those paths — one you wrote, or a link into your dotfiles — aborts the install with
an error instead of being overwritten; move it aside to let orch manage it. Edit the
repository copies, not the installed ones: `orch status` reports a hand-edited copy as
drift, and the next install restores it.

The shared policy is version-controlled in
[`internal/install/agent-instructions.md`](internal/install/agent-instructions.md).
It contains agent-neutral coding expectations for exploration, structure,
testing, security, credentials, Git/GitHub workflows, documentation, and durable
memory. Guidance that only applies to particular work, such as reviewing a pull
request or writing Go and Python, lives in
[`internal/install/policy/`](internal/install/policy/). It installs to
`~/.config/orch/policy/`, and the shared policy points agents at each file
before that work, so it isn't loaded into every session. Plain files are used
instead of skills because every harness reads the shared policy. Repository `AGENTS.md` loads that policy and adds this
project's conventions; repository `CLAUDE.md` imports `AGENTS.md`.
It is embedded in the CLI at build time. `orch install` writes that policy to
`~/.config/orch/AGENTS.md`; it never seeds the policy from a user's global files.
Change the repository source, rebuild or upgrade the CLI, and rerun install to
publish policy updates locally. The installed copy is Orchestrator-owned;
`orch status` detects missing or modified content against the policy shipped
with the running CLI, and recommends reinstalling to repair it.

Claude's global `~/.claude/CLAUDE.md` and Gemini's `~/.gemini/GEMINI.md` import
the installed shared file. Codex's global `~/.codex/AGENTS.md` explicitly directs
the agent to read it at session start; Codex does not expand Claude-style `@`
imports. An existing nonempty `AGENTS.override.md` receives the directive instead
because Codex loads it preferentially. Personal instructions remain in each
harness's entry file; only its Orchestrator-managed reference is updated.
Existing files, including the installed shared policy, are backed up before
modification. Backups are written owner-only (`0600`) into owner-only
directories, because harness config can carry OAuth state and MCP server `env`
blocks and these copies are retained indefinitely. Backups written by releases
before this change used `0644` under `0755`, and tightening is not retroactive —
clean up any you still have with
`chmod -R go-rwx ~/.local/share/orch/backups` (or the same path under
`$ORCH_DATA_DIR` / `$XDG_DATA_HOME/orch` if you set either). If upgrading an earlier installation that copied personal rules
into the shared file, recover those rules from its backup into your personal
harness entry files before relying on the replacement policy.

These paths assume the standard harness homes; `--home` relocates the whole
install for testing. Custom `CODEX_HOME` locations are not currently resolved.
Codex hooks must be reviewed and trusted in Codex before they execute. Memory
search and ingest through MCP work independently of automatic hook execution.

When run inside the Orchestrator checkout (including subdirectories and linked
worktrees), `orch status` also reports **repository instructions** in text and
JSON. It checks `AGENTS.md`, `CLAUDE.md`, and the version-controlled shared policy
against the current committed revision and verifies that the committed Claude
entry is the standalone `@AGENTS.md` import. Missing or edited files are reported as drift, with advice to review
local edits before restoring the affected file from `HEAD`, or to commit an
intentional instruction change. A committed broken import needs correction and
a new commit. Status is read-only; `orch install` repairs global wiring, while
Git owns repository guidance. Outside this repository the project check is not
applicable. These checks compare files on disk; they do not verify that a running
agent has loaded the latest instructions.

## Session memory

`orch-hook` gives every wired session durable memory without the agent asking for it.

- **Prompt enrichment.** On every prompt, `user-prompt-submit` searches the store and
  prepends the strongest hits (`hooks.auto_rag.*` sets the count, score floors and size).
  Question-shaped prompts need `min_score` (default 0.25). Instructions such as "fix the
  flaky test" need the stricter `min_score_statement` (default 0.45), since they say less
  about what they need. A chunk already injected earlier in the session is not injected
  again; that memory is a small owner-only file per session, removed when the session
  ends. Hits from the project you are working in come first: the hook resolves the
  session's working directory to its repository (linked worktrees count as their main
  checkout) and searches that project's registered docs (`repo:<name>`, named from the
  `origin` remote, else the checkout folder) and its past sessions (`checkout:<path>`)
  before topping up the remaining slots from the whole store.
- **Updates mid-session.** Each prompt also reports, once, what finished since the last
  one: this session's subagents and background queue tasks, as an "Orchestrator updates"
  block ahead of any RAG context. The first prompt skips what the SessionStart digest just
  showed, a long absence is summarized past 10 lines, and a queued task's own session gets
  none. PR babysitter changes stay digest-only, since they refresh every few minutes.
- **Resume brief after compaction.** When Claude Code compacts a long session, the next
  `session-start` adds a short brief to the context: the working directory, branch,
  uncommitted files and last three commits, plus the last request and reply verbatim
  from the checkpoint `pre-compact` saved (redacted like everything else). The harness's
  own summary keeps the gist; this restores the live state it can't see.
- **Session capture.** `session-end` writes a record under the data directory and ingests
  the summary and transcript as `session_record` / `session_transcript`, tagged with the
  main checkout's absolute path. Sessions never receive the remote-derived `repo:` tag:
  a repository's own config chooses its remote, so it could otherwise claim another
  project's name and have its session content promoted there. Transcripts, records,
  checkpoints, and subagent completion summaries are passed through secret redaction
  first, as repo docs, web sources, and native config already were. Redaction is
  pattern-based (private keys, cloud and vendor API keys, GitHub tokens, JWTs); it does
  not catch every secret shape, such as bare `PASSWORD=` lines.
- **Session summaries.** The record starts as a short excerpt. orchd then replaces it,
  in the store and on disk, with a model-written summary: outcome, decisions, files and
  changes, and open items. It runs `claude -p --model haiku` in `--safe-mode` with no
  tools and its own system prompt, so no `CLAUDE.md`, hooks or MCP servers load. A
  summary costs a fraction of a cent, and the summarizer's own session can't trigger
  another summary. Session end never waits for it: requests queue (up to 32) for the
  `session-summarizer` subsystem, and a failed summary keeps the excerpt. Settings are
  `sessions.summarize.enabled` (default on), `.model` (default `haiku`) and `.timeout`
  (default `2m`).
- **Repository docs.** Registered repos and owners have their markdown ingested as
  `repo_doc`. Directories named in `ingest.exclude_dirs` are never walked. The default
  list covers third-party and generated trees whose READMEs would otherwise crowd out the
  project's own docs: `vendor`, `node_modules`, `third_party`, virtualenvs, `dist`,
  `build`, `target` and similar. Names match exactly. Setting the list replaces the
  default, for example `orch config set ingest.exclude_dirs "vendor,node_modules,gen"`,
  and `.git` is always skipped. Excluding a directory stops new ingests only; docs
  already indexed from it stay until removed with `orch rag forget <source-uri>`.

## Status and restart

`orch status` is the whole-system sitrep: the `orchd` service, every supervised subsystem
(with its last tick and restart count), the vector store, the on-disk stores and their
freshness, harness wiring drift, and the daemon-independent preconditions. It also
reports, per harness, how much instruction text loads into every session: the entry
file plus everything it imports, in bytes and estimated tokens. That keeps the always-on
context cost visible as instructions grow. It stays useful
with `orchd` down — which is when you usually run it — and always exits zero. Use
`orch check` when you want a pass/fail gate with a non-zero exit, and `--json` when you
want to script it.

`orch restart` restarts the whole system by bouncing the `orchd` process through its
service manager, then waits for it to answer again. Naming a component restarts just that
one **in place**, without dropping the socket, the warm embedder, or the other subsystems:

```sh
orch status                      # full report (add --json for scripts)
orch restart                     # everything: bounce orchd (== orch restart orchd)
orch restart --list              # what can be restarted, per the running daemon
orch restart sources-scheduler   # one subsystem, in place
orch restart subsystems          # every subsystem, in place (no process bounce)
```

Subsystems are `qdrant` (binary mode only), `workflow-worker`, `sources-scheduler`,
`docs-scheduler`, `pr-babysitter`, `routing-evaluator`, `session-summarizer`, and
`self-update`. Every restart is idempotent: a
component that is already stopped is simply started.

### Picking up a new build

Upgrading `orch` replaces the binaries on disk but leaves the running `orchd` serving the
old code. The `self-update` subsystem closes that gap by itself: it watches the path the
service manager launches `orchd` from and, within a minute of a new build appearing there,
exits — and the KeepAlive supervisor behind `orchd` (`brew services`, or the launchd agent
`orch install` writes) starts the new binary. Nothing in flight is lost: the shutdown
drains the workflow worker, and agent tasks are durable, so they resume under the new
build.

`orch status` reports the `build` line as `drifted` when `orchd` is nonetheless still
running an older version than the installed `orch` — which means the self-restart did not
happen, typically because `orchd` was started by hand with nothing supervising it. `orch
restart` is the manual way to close it.

## Background tasks

`orch dispatch "<task>"` (or the `queue_dispatch` MCP tool) records a durable task. With the
queue worker enabled, orchd runs each task as a headless Claude Code session:
`claude -p --agent <agent>` (default `dev`) in the dispatching directory, with the task as
the prompt. The agent's final message becomes the result: `complete`, `failed`, or `blocked`
when it begins with `BLOCKED:`. Read results with `orch tasks` / `orch task <id>` or
`queue_list` / `queue_read`; finished tasks also appear in the next session's digest.
Each finished task records its outcome: cost, run time, turns, the models that did the
work, its Claude Code session ID, and any pull requests its result links to.
`orch tasks --stats` summarizes those per agent (success rate, average run time, total
cost) over the most recent 500 tasks.

The worker is **off by default** because it starts agent sessions unattended:

```sh
orch config set queue.worker.enabled true
orch restart                       # the worker starts with orchd
```

While it is off, dispatched tasks stay `pending` and run once it is enabled. Other knobs:

- `queue.worker.permission_mode`: default `auto`. Only `auto`, `dontAsk`, `acceptEdits` and
  `plan` are accepted, because each decides without a human. `bypassPermissions` is refused,
  and so is any other mode; a task run with one fails instead of starting.
- `queue.worker.timeout`: default `60m` per task.
- `queue.worker.max_concurrent`: default 1.
- `queue.max_active`: default 20. Dispatch fails once that many tasks are pending or
  running.
- `queue.worker.claude_bin`: found on PATH or in the usual install locations when empty.
- `queue.worker.allowed_tools.<harness>`: permission rules a queued session gets without
  asking, kept per harness because each spells its rules differently. Only `claude` is
  used today; its default is a read-only set (`git status`/`diff`/`log`/`show`/`branch`,
  `ls`, `go test`, `go vet`, `gh pr view`/`checks`/`diff`, `orch search`/`status`). They
  matter most under `dontAsk`, where anything not allowed is refused. Set a list with
  `orch config set queue.worker.allowed_tools.claude "Bash(make test:*),Bash(ls:*)"`.
  Rules containing a comma, or beginning with `-`, are ignored. Your own Claude Code
  `permissions` still apply on top, and the dispatch tools stay denied whatever is listed.

Behavior to know about:

- **Work is verified before it counts as complete, in repositories you trust.** A check
  runs the repository's own code outside the agent's permission checks, so it only runs
  in repositories listed in `queue.worker.verify.repos` (main checkout paths, `~`
  allowed), for example `orch config set queue.worker.verify.repos "~/code/willesq/orchestrator"`.
  At dispatch, orchd records the HEAD of every checkout of the task's repository. When the
  agent finishes, it tests each checkout whose HEAD moved, such as the worktree `gittree`
  created, using a detected command:
  - a make `test` target (`GNUmakefile`, `makefile` or `Makefile`, as make reads them)
  - else `go test ./...` for a Go module
  - else `cargo test`
  - else the `package.json` test script

  Python isn't guessed. Checks run with a minimal environment (home, PATH and toolchain
  caches; no tokens or SSH agent) and at most five per task. If a check fails, the
  agent's session is resumed once with the output, fenced and labelled as untrusted, to
  fix it (`queue.worker.verify.retries`, default 1). If it still fails, the task is
  recorded as `failed` with the output attached. Unlisted repositories, tasks that moved
  no checkout, and projects with no recognizable test command are recorded as `skipped`
  and keep their status. The outcome appears as a `verify:` line in `orch task <id>`.
  `queue.worker.verify.enabled false` turns verification off, and
  `queue.worker.verify.timeout` (default `15m`) caps each command.
- **No automatic retries.** A failed run has already had side effects, such as pushed
  branches or opened PRs, so it is recorded as `failed` rather than repeated.
- **Restarts resume.** Stopping orchd or restarting the worker ends in-flight sessions
  (their whole process group) without waiting them out. Once orchd is back, each
  interrupted task resumes its own Claude Code session (`--resume`) instead of starting
  over. Expect a delay of a few minutes while its lease lapses.
- **Bounded fan-out.** A queued session runs with `ORCH_TASK_ID` set. `queue_dispatch` and
  `orch dispatch` refuse inside it, and the session is started with both denied through
  `--disallowedTools`. Those are first lines only, because a determined session could reach
  orchd another way; the hard bound is `queue.max_active`.
- **Environment.** orchd runs under launchd's minimal PATH. The session gets the CLI's
  directory, Homebrew, `~/.local/bin` and `~/go/bin` prepended; the agent's own shell
  commands load your login profile as usual.
- **What the session inherits.** It gets orchd's environment minus the vector store's API
  key.
- **Redaction and size.** Task bodies (capped at 64 KiB) and results (capped at 64 KiB) are
  redacted with the same rules as ingested content.
- **The digest stays clean.** The next session's digest names each finished task and its
  status but never its result text. That text was written by an unattended agent, possibly
  steered by an untrusted repository, so it is read deliberately with `queue_read`.

## Model registry

orch keeps a registry of the models each coding-agent CLI on this machine can run.
[Routing](#routing) picks from it per task kind.

```sh
orch model refresh                       # discover from each CLI's local cache
orch model list [--provider codex] [--state enabled]
orch model enable codex/gpt-6.1-sol      # the only way a discovered model becomes routable
orch model disable claude-code/fable
orch model tier codex/gpt-6.1-sol fast   # frontier | balanced | fast; refresh keeps it
orch provider list | enable codex | disable codex
```

- **Discovery is local and credential-free.** Claude Code contributes its built-in aliases
  (`opus`, `sonnet`, `haiku`, `fable`, which follow each family's latest release) plus any
  extra models it lists in `~/.claude.json` under `additionalModelOptionsCache`. Codex
  contributes `~/.codex/models_cache.json`, including each model's reasoning-effort levels.
  Only those model fields are read; the account data beside them never is.
- **Discovery never enables.** A discovered model is `discovered` and inert until you enable
  it, so a vendor shipping a model cannot redirect your work. The compiled-in Claude aliases
  start enabled. A model its vendor marks hidden is listed but cannot be enabled.
- **Your choices stand.** Refresh updates names, descriptions and effort levels but keeps the
  state and tier you set. `claude-code` starts enabled because the worker already runs it;
  `codex` starts disabled. A provider whose CLI is not on PATH reads `unavailable` until it
  is found again, and then returns to the state it had before. A model you enabled that its
  vendor later hides stays enabled but is not routed to while hidden.

### Routing

Each dispatched task can name a **task kind** (`code`, `review`, `docs`, `research`, `ops`,
`classify`, `test`, `adversarial`). The routing table ranks, per kind, the enabled models
that kind runs on.

```sh
orch routing evaluate [--explain code]    # propose a table from the policy; writes nothing
orch routing apply                        # write it; the replaced table is kept
orch routing rollback                     # restore the table before the last change
orch routing show
orch routing set docs claude-code/sonnet  # pin a kind by hand; apply leaves it alone
orch routing clear docs                   # drop a kind's routes and pin

orch dispatch --kind code "<task>"        # run on the model ranked first for code
orch dispatch --model claude-code/opus "<task>"
```

- **The policy is config.** `routing.kinds.<kind>` sets the tier each kind prefers (defaults:
  frontier for code, review and adversarial; balanced for research, docs, ops and test; fast
  for classify). `routing.providers` orders the providers that may be proposed. A provider
  that is not listed is never proposed. `routing.efforts.<kind>` asks models that declare
  effort levels for one. `routing.ranks` (default 3) sets how many fallbacks each kind keeps.
- **Ranking starts from declared capability.** With no evidence, models rank by distance
  from the wanted tier, then provider order, then alias over pinned ID. Every route reports
  how many observed outcomes back it (`evidence_n`). Confidence is `low` below 5, `medium`
  below 20, and `high` from 20.
- **Outcomes move ranks.** Each routed task counts once, by its best label:
  - your rating: `orch task <id> --rate good|rework|bad`;
  - else the fate of the PRs it opened, which the PR babysitter checks once they close (merged
    as the task left it is good; merged after more authored commits is rework; closed unmerged
    is bad). A PR counts only if you opened it after the task was dispatched, so a session
    cannot claim someone else's merged work. Checks are capped per task and per sweep, and
    stop after 30 days;
  - else `bad` if it failed.

  A task that only completed carries no verdict, and turns and duration never count, so speed
  cannot stand in for quality. Once a model has `routing.min_evidence` (default 5) outcomes
  on a kind, its success rate is blended with the tier prior, weighted by how many there are.
  `orch routing evaluate` then proposes the change. Evidence reaches dispatch only through a
  table you apply; with no table route, dispatch ranks by declared capability alone.
- **A weekly evaluator proposes.** The `routing-evaluator` subsystem runs model discovery and
  evaluation every `routing.evaluate.interval` (default `168h`; `routing.evaluate.enabled`
  turns it off). It reports new models and pending changes in the next session's digest,
  once per distinct proposal, naming each change. It applies nothing unless
  `routing.auto_apply` is `true`. Even then it holds back any run whose discovery failed and
  any change that drops a route, keeps pinned kinds, always reports what it applied, and
  `orch routing rollback` undoes it. Discovery still never enables a model.
- **Routes are fixed at dispatch.** orchd resolves the model when the task is queued and
  records it on the task (`orch task <id>` shows it), so a resumed task keeps its model. The
  worker runs Claude Code, and Codex once [Codex runs](#codex-runs) are enabled. A route to
  any other provider falls to the next rank. A kind with no routable model runs on the
  agent's own model.
- **Only you change it.** Inside a queued task, `orch routing apply|set|clear|rollback`,
  `orch model enable|disable|tier`, `orch provider enable|disable` and `orch task --rate`
  refuse. This stops a session that an untrusted repository is steering from casually
  redirecting later work or grading its own. It is a guard, not a boundary: a session runs as
  you and could reach orch.db another way. A pending task whose model
  you disable or hide before it starts runs on the agent's own model instead.
- **Sessions can ask.** The `model_recommend` MCP tool returns the model for a kind with its
  rationale and evidence count. `queue_dispatch` takes `task_kind` and `model`. The
  dispatcher agent picks a kind, and `orch-dispatch` passes a Claude alias to the `Agent`
  tool's `model`.
## How text is chunked

Ingested text is split into chunks of at most `embedder.chunk_tokens` model tokens (default
110) before it is embedded, counted with the model's own tokenizer.

- **Chunks fit what the model reads.** The embedding model reads only the first 128 tokens
  of its input, 126 after its special tokens. Larger `chunk_tokens` values are capped there,
  so nothing in a chunk goes unseen by search.
  - Earlier versions sized chunks at 400 *words*. Across typical repositories that was about
    900 tokens, so search saw under a quarter of each chunk.
  - The old `embedder.chunk_size` and `embedder.chunk_overlap` keys are no longer read.
- **Markdown follows its sections.** A document with `#` headings outside code fences is
  split at headings. Its line breaks, lists and fenced code blocks are kept.
  - Small sections in the same subtree share a chunk. A section too large for one splits at
    paragraph breaks, and never inside a fenced block unless the block alone is too large.
  - `embedder.markdown_sections: false` turns this off.
- **Each chunk carries its context.** A chunk starts with its ancestor headings, for example
  `[section: Install › Homebrew]`, counted against its size and capped at half of it. Its
  stored `heading` field names the deepest path all its sections share.
- **Plain text uses overlapping windows.** It is split into token windows sharing
  `embedder.chunk_overlap_tokens` (default 16) with their neighbours.
- **Long words are split.** A single "word" too large for a chunk (a long rule, a table
  separator, a URL) is split into pieces.
- **Titles are bounded.** Headings are capped at 120 characters and cleaned of control
  characters, so a hostile page cannot multiply its size by repeating a huge title.
- **Existing content** keeps its old chunks until it is ingested again.

## Measuring retrieval

`orch rag eval` scores search against a golden set: queries you actually ask, each paired
with the sources that should answer it. Use it to judge a change to chunking, ranking or the
embedding model by number.

```sh
orch rag eval add "how do I install orch with homebrew" repo://willesq/orchestrator/README.md
orch rag eval add "routing evidence" "repo://willesq/orchestrator/docs/*" --tag repo:orchestrator
orch rag eval --save before        # run every query, keep the result
# ...re-ingest, change chunking or the model...
orch rag eval --compare before     # deltas, plus which queries got better or worse
```

- **The set** is YAML in the orch config dir (`rag-eval.yaml`; `--set` picks another). A
  trailing `*` on an expected URI matches a prefix.
- **Scoring:** each query goes through orchd's normal search. Hits are counted by distinct
  source, so several chunks of one document count once. The report gives hit@1, recall@k
  (`--top-k`, default 10) and MRR. `--json` prints the report for scripts.
- **Saved runs** live in the data dir under `eval/`.

## Build from source

The ORT embedder is cgo: it links a prebuilt `libtokenizers.a` at build time and loads
`libonnxruntime.dylib` at runtime.

```sh
brew install onnxruntime
# stage the tokenizers static lib for your arch (daulet/tokenizers release):
#   build/lib/darwin-arm64/libtokenizers.a
make build      # -> build/bin/{orch,orchd,orch-hook}
make test       # pure-Go tests (no ORT tag)
make cover      # per-file coverage gate (>= 80%)
make proto      # regenerate the orchd gRPC stubs after editing the .proto
```

## Architecture

See [`docs/GO_REWRITE_PLAN.md`](docs/GO_REWRITE_PLAN.md) for the full design: the `orchd`
process model, the embedder parity, durable workflows (go-workflows), the source registry,
the multi-harness install adapters, and the packaging story.

## Command surface

`orch check | config | daemon | dispatch | ingest | install | mcp | model | ping | provider | prs |
qdrant | repo | restart | routing | search | source | status | version`
