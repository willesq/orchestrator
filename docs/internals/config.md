# Configuration

orch's settings live in `orch.db` and are changed in Orch.app's Settings pane, through orchd's
`GetConfig` and `SetConfig`. There is no settings file and no environment override: `ORCH_<KEY>`
variables are not read. The location variables (`ORCH_DATA_DIR`, `ORCH_CONFIG_DIR`, `ORCH_SOCKET`)
still say where orch keeps its files, `orch.db` among them.

`internal/config` resolves the compiled defaults, then the rows of `config_values` (migration
`0022`: key, JSON value, `updated_at`, `updated_by`). Only values that differ from a default are
stored, so a default changed in a later release reaches everyone who never set it, and setting a
value back to its default deletes its row. Every write validates first (`ApplyStore`, the checks
below) and lands in one transaction: related keys change together, all or nothing. A row that names
no setting, or no longer passes its checks (only possible by editing `orch.db` by hand), is skipped
and logged by key, never by value, and the default applies.

- **Readers.** `config.Load` opens `orch.db` read-only and never migrates it, so orch-hook (on every
  prompt) and the CLI's read commands can call it; with no store yet, or one older than migration
  `0022`, the defaults apply. orchd reads through its own connection (`LoadStore`).
- **Writers.** Orch.app through `SetConfig` (`updated_by` `app`). orch's own maintenance writes
  record what they switched through `config.Save`: a hybrid migration's collection switch and backup
  drop (`orchd:migrate`), `orch qdrant migrate` and `cutover` (`cli:<command>`). `orch config reset`
  (`cli:reset`) is the way out when orchd cannot run at all.
- **CLI.** `orch config get <key>` and `orch config list` read; `orch config reset <key>...` or
  `--all` puts settings back to their defaults and names the keys it reset, refused inside a queued
  task. There is no `orch config set`.
- **Accepted risk.** `orch config reset` writes `orch.db` itself, so orchd's process-ancestry guard
  (daemon.md, "Queued callers") does not cover it: only `ORCH_TASK_ID` and the Claude Code deny rule
  stop a queued task, and a task can clear the variable. Resetting a sensitive key can widen what
  agents may do. This is no wider than before (`orch config set` had the same gate, and a task can
  write `orch.db` or could edit `config.yaml` directly); the harness sandbox is the boundary.

### The old config.yaml

Releases before this kept settings in `<config>/config.yaml`. At startup orchd imports it once
(`ImportLegacy`): every value that differs from today's default is stored as it is, with
`updated_by` `import`, even one an older orch wrote only because it saved every key, since dropping
it would change what runs (a worker that was off would turn on). The queue limits are imported as
the old loader clamped them (`max_active: 0` ran as 50). Invalid and unknown keys keep their
defaults and are logged by name. A `config_import` row records that the import ran, so it never runs
twice; a first start without the file records it too, so a config.yaml written later (by an older
orch, say) is never imported over settings confirmed in the app. A file that does not parse is not
imported, and orchd tries again at the next start.

The file is left in place and never read again. `GetConfig` reports it in `legacy_file` while it
exists, and `RemoveLegacyConfig` removes it: always that one fixed path, and only once the import
ran. It first copies the file (owner-only) to `<data>/backups/config.yaml.<UTC time>`, since its
comments and any keys the import could not keep live only there; a symlink is removed as a link,
its target untouched. Orch.app offers this as a quiet row under Settings → Overview.

### When a setting stops orchd

Settings can only be changed through orchd, so a setting that stops orchd's services from starting
(a model folder it cannot create, a port in use, a storage path) must not take orchd down with it.
If anything after the config is read fails (the Qdrant key, the embedding model, the workflow
store, adding or starting subsystems), orchd stops what started and serves only `Ping`,
`GetConfig`, `SetConfig`, `RemoveLegacyConfig` and `Restart`, with the failure in `GetConfig`'s
`startup_error`; every other RPC is unimplemented. Orch.app shows the error at the top of Settings.
Once the setting is fixed, restarting orchd (the app's restart, or `Restart`, which exits for the
service manager to start orchd again) tries the full start. orchd also exits to try again after
10 minutes, so a failure that clears by itself (the network down during the first model download)
does not leave its services down until someone notices. When orchd cannot run at all,
`orch config reset` reaches the store directly.

| Key | Default | Meaning |
|---|---|---|
| `embedder.model_dir` | `<data>/models/minilm` | embedding model directory |
| `embedder.onnx_library_dir` | via `brew --prefix onnxruntime` | ONNX Runtime dylib directory |
| `embedder.idle_unload` | `10m` | unload the model after this idle time |
| `embedder.chunk_tokens` | `110` | chunk size in model tokens (capped at 126, the model's window) |
| `embedder.chunk_overlap_tokens` | `16` | overlap between plain-text windows |
| `embedder.markdown_sections` | `true` | chunk markdown by heading |
| `rag.qdrant.mode` | `binary` | `binary` (orchd runs it), `docker`, or `external` |
| `rag.qdrant.url` | `http://127.0.0.1:6333` | used unless binary mode (then `127.0.0.1:<http_port>`) |
| `rag.qdrant.collection` | `orchestrator` | active collection |
| `rag.qdrant.backup_collection` | empty | set by migration for rollback |
| `rag.qdrant.api_key_env` | `QDRANT_API_KEY` | env var that overrides the generated key |
| `rag.qdrant.version` | `v1.17.1` | pinned engine release |
| `rag.qdrant.storage` | `<data>/qdrant` | engine storage |
| `rag.qdrant.http_port`, `grpc_port` | `6333`, `6334` | engine ports |
| `rag.search.mode` | `hybrid` | or `dense` |
| `rag.search.rrf_k` | `60` | reciprocal rank fusion constant |
| `rag.search.keyword_weight` | `0.5` | keyword list's share of the fusion |
| `rag.search.ranking` | `weighted` | scale fused scores by source type and recency, or `plain` |
| `rag.search.recency_half_life_days` | `180` | days for recency to fall halfway to the floor; 0 turns recency off |
| `rag.search.recency_floor` | `0.85` | least recency scales a score by (0–1) |
| `rag.search.source_weights` | `session_transcript: 0.85`, `web: 0.9`, `session_record`/`decision: 1` | per source type; set types override these defaults, other types weigh 1; values outside (0, 10] are ignored |
| `rag.search.per_source` | `2` | most hits one source returns to a search that sets no cap (every search: MCP, CLI and prompt context, which asks for 1); 0 = no cap |
| `hooks.auto_rag.enabled` | `true` | inject memory into prompts |
| `hooks.auto_rag.top_k` | `3` | hits injected |
| `hooks.auto_rag.min_score` | `0.25` | cosine floor for questions |
| `hooks.auto_rag.min_score_statement` | `0.45` | cosine floor for other prompts |
| `hooks.auto_rag.max_chars` | `2000` | injection size cap |
| `hooks.auto_rag.min_prompt_chars` | `12` | shorter prompts get nothing |
| `hooks.auto_rag.gate` | `evidence` | a prompt must name a topic and a hit must show evidence for it ([hooks.md](hooks.md#prompt-context)); `off` leaves the score floors alone; any other value means on, so a typo cannot quietly turn it off |
| `hooks.auto_rag.format` | `pointers` | one line per hit with its source, or `full` text |
| `hooks.auto_rag.untrusted` | `mark` | inject web pages and tool output labelled untrusted, or `exclude` them |
| `ingest.exclude_dirs` | vendor, node_modules, bower_components, third_party, Pods, .venv, venv, \_\_pycache\_\_, .tox, .terraform, .next, dist, build, target | never walked; `.git` always skipped |
| `sources.known_roots` | `[~/code]` | where `<owner>/<repo>` checkouts live |
| `sources.scan_interval` | `5m` | sources scheduler tick |
| `feeds.chat.direct_messages` | empty | chat tenants (`slack:<id>`, `teams:<id>`) whose DMs and group DMs a chat feed stores; found only by the operator's `orch search --direct-messages` ([memory.md](memory.md#chat-feeds)); sensitive |
| `feeds.chat.containers` | empty | per tenant, the channel and meeting IDs a chat feed stores; nothing else is stored (DMs follow `direct_messages`); sensitive. Tenant keys are folded to lower case when loaded; tenants match regardless of case |
| `docs.scan_interval` | `1m` | docs scheduler tick |
| `docs.repo_cache` | `<data>/repos` | clones of GitHub owners' repos |
| `docs.default_refresh` | `6h` | per-owner refresh cadence |
| `docs.include_archived`, `include_forks` | `false` | repo filters |
| `pr_babysitter.enabled` | `true` | PR cache refresh |
| `pr_babysitter.poll_interval` | `10m` | sweep cadence |
| `github.isolated_account_hourly_budget` | `1000` | GitHub API calls an hour for each isolated space's own gh account; past it its docs sync and PR watching wait (0: no cap; SP-074.2) |
| `pr_reviews.poll_interval` | `2m` | how often each PR watched for review is read; at least the unit's 15 s tick |
| `sessions.summarize.enabled` | `true` | model-written session summaries |
| `sessions.summarize.model` | `haiku` | model for summaries |
| `sessions.summarize.timeout` | `2m` | per summary |
| `sessions.command_log.retention` | `7d` | how long an ended session's [command log](daemon.md#command-log) records are kept: `3d`, `7d` or `30d`; refusals are kept 90 days, a running session's log is never expired (only cut to `max_session_mb`). Sensitive; another value keeps `7d` |
| `sessions.command_log.max_total_mb` | `500` | cap on `toollog.db`'s pages in use; the sessions that ended first lose their records to fit, never a running one's and never refusals under 90 days, so the file can stay over it. 100 to 100000; sensitive |
| `sessions.command_log.max_session_mb` | `50` | cap on the records of each chain whose owner still runs (a session, a queued task or an unpinned harness process); its oldest records other than refusals are deleted to fit (SP-073). 10 to 1000; sensitive; another value keeps `50`; read when orchd starts |
| `queue.max_active` | `20` | pending + running tasks; dispatch fails beyond. 1 to 50, never below `max_concurrent` |
| `queue.worker.enabled` | `true` | run dispatched tasks (never with `bypassPermissions`; sessions cannot dispatch more) |
| `queue.worker.claude_bin` | found automatically | Claude Code binary |
| `queue.worker.permission_mode` | `auto` | `auto`, `dontAsk`, `acceptEdits` or `plan` |
| `queue.worker.timeout` | `60m` | per task |
| `queue.worker.max_concurrent` | `1` | parallel tasks, 0 to 50; 0 turns the worker off |
| `queue.worker.allowed_tools.claude` | read-only git/ls/go test/go vet/gh pr view/orch search set | rules granted without asking |
| `queue.worker.verify.enabled` | `true` | test a task's changes before completing it |
| `queue.worker.verify.timeout` | `15m` | per check |
| `queue.worker.verify.retries` | `1` | resumes with the failure output |
| `queue.worker.verify.repos` | `[]` | repositories trusted to run their own tests |
| `queue.worker.codex.enabled` | `false` | run tasks routed to Codex |
| `queue.worker.codex.bin` | found automatically | Codex binary |
| `queue.worker.codex.sandbox` | `workspace-write` | or `read-only`; full access refused |
| `queue.worker.codex.network` | `false` | network inside the sandbox |
| `queue.worker.codex.mcp_allow` | `[orch_rag]` | MCP servers kept for Codex runs |
| `queue.worker.codex.allow_project_config` | `false` | honor a repo's `.codex` config |
| `routing.kinds.<kind>` | frontier: code, review, adversarial; balanced: research, docs, ops, test; fast: classify | preferred tier |
| `routing.providers` | `[claude-code, codex]` | providers that may be proposed, in order |
| `routing.efforts.<kind>` | none | effort level to request |
| `routing.ranks` | `3` | routes kept per kind |
| `routing.min_evidence` | `5` | outcomes before evidence moves a rank |
| `routing.evaluate.enabled`, `interval` | `true`, `168h` | evaluator subsystem |
| `routing.auto_apply` | `false` | evaluator only proposes |

## Spaces policy

The spaces policy ([the spaces plan](../spaces/spaces-features.json), D-25) is not a setting in
`config_values`: it is nested, and it must be reviewed before it takes effect, which `SetConfig`
does not do. `internal/spaces` keeps it in two tables of `orch.db` (migration `0023`), plus the
GitHub org bindings below (migration `0024`):

- **`spaces_revisions`:** every saved policy, as canonical JSON with its sha256, who saved it,
  and whether that was the operator (`by_operator`). `SetSpacesDraft` (`orch spaces draft`,
  JSON on stdin) adds one. Saving the newest revision's policy again, as the same kind of caller,
  returns that revision. Saving changes nothing that is enforced, and an invalid draft is saved
  too, with its problems in the reply, so it can be fixed in steps. A policy is limited to 64
  spaces, 256 entries in any one list, 512 roots, 1024 bytes per value, 64 folders per path and
  1 MB in all. Validation reports at most 100 problems. orchd caches whether the newest draft
  validates and never validates a draft at startup. Together these keep one saved draft from
  stalling orchd.
- **`spaces_applied`:** every apply, newest last, with the sha256 of what was applied.

`ApplySpaces` (`orch spaces apply --revision N`) enforces revision N only when all of these hold:
it is the newest revision, so the operator applies exactly what they reviewed (`DiffSpaces`,
`orch spaces diff`); it validates; and, if an agent session saved it, the operator passed
`accept_agent_draft` (`--accept-agent-draft`). Otherwise the applied revision stays as it was.
The stale check alone cannot tell the operator's draft from one a session saved a moment later,
so the diff names who saved the draft and shows every value it adds or removes.

Apply and diff are the operator's alone. They are refused to queued tasks, to any caller with an
agent harness among its ancestors, and to a caller orchd cannot place or whose ancestry it cannot
read ([daemon.md](daemon.md#queued-callers)): the diff names every job's orgs and roots.
`SetSpacesDraft` is open to interactive sessions (agents may propose a policy, D-25) but refused
to queued tasks.

**Enforcement and drift.** orchd loads the applied revision once at startup (`spaces.Open`) and
afterwards changes it only through `ApplySpaces`. At startup only the newest apply counts. Its
revision must still hash to the sha256 recorded in both tables and still pass the structural
checks. The filesystem rule (a terms file in a git tree) was checked at apply, and a later change
on disk does not undo an apply. When the newest apply fails, orchd never falls back to an older
one, which would let whoever broke it choose the policy. Instead the engine is broken:
`Current` returns `ErrBroken`, enforcement must fail closed, and status says so until a revision
is applied again. `Status` reports drift, which `orch status` shows in a `spaces` section once a
revision exists:

- `draft`: a saved revision differs from the applied one.
- `changed_outside_orchd`: the newest apply row, or the enforced revision's document, in
  `orch.db` no longer matches what orchd loaded. Editing `orch.db` therefore changes nothing until
  the next start. At that point a document edited without its hash breaks the engine (fail
  closed). One edited together with both recorded hashes would load: the same accepted risk as
  the settings above, where a process that can write `orch.db` is the harness sandbox's to stop.

`Status` errors never name a space, root or org, since tasks may call `Status`; `DiffSpaces`
carries the details.

**GitHub org bindings** (`spaces_org_bindings`, `internal/spaces/orgs.go`). A deleted GitHub
login can be registered again by someone else; the account's node ID cannot be reused, so a
binding records the node ID and trusts that. Before `ApplySpaces` takes the write lock, it looks up
every org the revision binds through `Env.LookupOrg`. orchd's lookup is
`github.LookupAccount`: `gh api users/<login>`, which answers for orgs and users alike. The lookup
must return the same login, compared case-insensitively, and a usable node ID. If any lookup fails,
the apply is refused (`Unavailable`) and nothing changes. The lookups are skipped when the apply
would be refused anyway (a stale revision, or an agent's draft without `accept_agent_draft`).
Inside the apply's transaction each bound org's row is written: keyed by the lowercased login,
with GitHub's spelling, the node ID, the revision and the time. Rows of orgs the revision no longer
binds are kept, so dropping an org and adding it back later cannot bind a different account
unconfirmed. An org whose node ID differs
from its existing binding is refused with `ErrOrgChanged` (`FailedPrecondition`) unless
`confirm_github_orgs` names it (`--confirm-github-org`). So an apply made for another reason
never accepts a changed org by accident. Re-binding clears a suspension.

Each docs sync of an owner first calls `Engine.CheckOrg` (`docs.SyncOpts.CheckOwner`), and calls
it again before each further repo it clones and writes, so an org whose identity changes during a
long sync has nothing more written (D-52). The sources refresh (before each checkout whose origin
names a GitHub owner), the PR watcher (once per owner each sweep, before caching its items) and
the review watcher (before starting a review and before recording a verdict) run the same check
(SP-023); a refused owner's content is not written. An owner
that no space binds syncs as before, with no GitHub call. For a bound owner, orchd looks it up
again. If the node ID differs from the binding, the binding is suspended: `suspended_at` and
`seen_node_id` are set, and orchd logs that a bound org changed. The log line names no org,
because tasks can read orchd's log. A suspended binding
stays suspended even if GitHub later returns the original node ID; only an apply clears it. While
an org is suspended, or was applied before bindings existed and has no row, `OrgSpace` and
`CheckOrg` return `quarantine` for its new content. A changed identity means the content may
belong to someone else, so rather than quarantining it the sync indexes nothing for that owner
(D-52): its result is `error`,
with a reason that points to `orch spaces diff`. A failed lookup also stops that owner's sync
without suspending. The reason is the same in both cases and has no lookup detail, since agent
sessions can list docs owners. `Status` adds an error that counts the unconfirmed orgs without naming them,
and `DiffSpaces` lists them (`unconfirmed_github_orgs`). Each lookup runs as the gh profile of the
space that binds the org (below): at apply, the draft's binding decides; at sync, the applied
policy's. So an isolated org cannot be applied until its space's profile is logged in, and the
refusal names the space.

**GitHub accounts per space** (SP-063, D-18; `spaces.GHProfile`, `internal/daemon/ghprofiles.go`).
A space may name a gh profile directory, `gh_config_dir`. orchd runs every gh call about an org
bound to that space with `GH_CONFIG_DIR` set to it, so the call runs as the account logged in
there. It also removes `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`,
`GITHUB_ENTERPRISE_TOKEN` and `GH_HOST`, which would otherwise override the profile
(`github.ProfileEnv`). The user logs each profile in themselves with
`GH_CONFIG_DIR=<dir> gh auth login`. orch checks a profile only with
`gh auth status --active --hostname github.com --json hosts`, using the login it reports, and then
lists that account's orgs (`gh api user/orgs`). Both answers are cached for five minutes, or one
minute after a failure. A check runs on its own 15-second deadline, and a caller's cancellation is
never cached, so a task calling `Status` and giving up early cannot make a working profile look
logged out. orch never reads files inside a profile directory and never runs
`gh auth token`; `internal/github`'s `TestNoCodeReadsGHCredentials` scans the source to keep it so.

Which login a call runs as:

- An org bound to a space with a profile: that profile, when it is usable. Usable means
  `github.CheckProfileDir` accepts the directory (an absolute path that resolves, after symbolic
  links, to an existing directory under the user's home, owned by the user, with no group or other
  permission bits) and `gh auth status` reports a working login (SP-074).
- An org bound to a space whose profile is not usable: no call at all. Its docs are not synced,
  its PRs and issues are dropped from the PR watcher, and its review posts, PR lookups, repo
  listing and identity checks are refused (`errGHProfileDir`, `errGHProfileLogin`). The reason the
  docs list and orchd's log carry names no space; the operator sees which in `orch status`.
- An org bound to a space without a profile (an isolated one included, D-59), an org no space
  binds, and a call about no org (`gh api user`): the default login, as before spaces. With one
  login the GitHub account is not a boundary between jobs (D-59).

A call a profile makes that GitHub answers with 404, 403, "Could not resolve to a …" or "Resource
not accessible" fails with `github.ErrNoAccess`, keeping gh's words; a rate limit is not marked.
`CheckProfileDir` also requires every folder between the home and the profile to be the user's
and not writable by others. Nothing retries it as another
login (SP-074.1.a2).

**Hourly budget** (SP-074.2, `ghBudget`). Every call orchd makes as an isolated space's own
profile counts toward `github.isolated_account_hourly_budget` (default 1000; 0 is no cap), in a
window that starts at the first call and lasts an hour. Once it is spent, that account's docs sync
(`docsClients` and each listing call), PR-watch calls and PR-outcome reads
(`RoutedBackground`) fail with `github.ErrQuota` until the window ends. The docs sync reports the
owner as not synced and keeps its chunks, and the PR watcher defers the account, keeping its
items. Other routed calls (reviews, lookups, landing) still run and are counted, up to twice the
budget (`hardCapFactor`), so nothing spends the account's whole rate limit. The check and the
count are one step under a lock. A `gh api --paginate` counts as one call whatever its pages. A
private or public space's profile and the default login are not budgeted. The first
refusal in each window is logged once, naming no space or directory. Counts live in memory and
start again when orchd restarts.

The call sites:

- **Routed calls.** PR lookups, the review watcher, landing's PR reads, placement's repo lookups
  and the GitHub tab's repo list go through `ghProfiles.Routed`. It reads the account from the call's
  `https://github.com/<owner>/…` URL or its `repos/`, `users/` or `orgs/` API path, and refuses a
  call that names two different accounts.
- **Review posts** use `reviews.NewGHPoster`, which picks the login from the repo's owner.
- **Landing** (`landing.Service.AccountFor`) requires the PR to be the profile account's own, and
  pushes as that account. The push runs with the profile's environment through
  `landing.GitWithEnv`, which drops the user's global and system git config and `SSH_ASKPASS`.
- **The docs owner picker** (`ListDocsOwnerRepos`) lists a space's org as its account, and refuses
  one whose space has no working profile.
- **Docs sync** uses `docs.SyncOpts.Clients`, which runs before `CheckOwner`. A profile's clone
  does not use `gh repo clone`, because git would then choose credentials from the user's SSH keys
  or keychain helper. It runs `git clone https://github.com/<owner>/<repo>.git` with gh, in the
  profile's environment, as the only credential helper (`credential.helper=!gh auth git-credential`).
  It also sets `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_CONFIG_NOSYSTEM=1`, and drops `GIT_ASKPASS`,
  `SSH_ASKPASS` and `GIT_CONFIG_*`, so no `insteadOf` rule, helper or askpass program can substitute
  another account.
- **The PR watcher** searches once as the default login and once as each usable profile. Each
  owner's items are kept only from the account its space routes it to, and are enriched as that
  account ([tasks.md](tasks.md#pr-babysitter)). As with the default login, a profile's items are
  kept only for a scope the user enabled, because tasks in any space can read the PR cache.
  `ListPRs` and `ListGitHubItems` leave out, for agents, the items of an owner bound to a space
  with its own profile (SP-074); the cache file itself is still readable by the user's processes.
- **GitHub scopes and the repo picker** (SP-074, `github.Service.Accounts`, `ghProfiles.Account`
  and `ScopeOwners`). Each org a space binds that its usable profile's account is or belongs to is
  added as a disabled scope, so an org only that account reaches can be turned on. Turning it on
  checks membership as that account, and is refused while that profile is refused.
  `ListGitHubRepos` lists its repos as that account. Both RPCs are open to agents, so an owner
  bound to a space with its own profile is left out of an agent's scope list, and an agent's repo
  listing for it answers `NotFound`, as for an org that is not the user's.
- **PR outcomes** (`newAttributor`) read each PR, and match its author, as the account of the PR
  owner's space (`evidence.Attributor.LoginFor`).

`Status` adds the problems to the spaces line. The operator (a caller that `refuseNonOperator`
would admit) sees each one with the space it concerns:

- a profile directory orch refuses, with why (not under the home, not owner-only, missing);
- a profile that is not logged in, with the exact `GH_CONFIG_DIR=<dir> gh auth login` to run;
- a profile whose account can see an org bound to a different isolated space (it is that org's
  login or a member of it), since account separation is then not effective;
- two isolated spaces whose profiles are logged in to the same account;
- the default login seeing the org of an isolated space that names its own profile, or being the
  same account as an isolated space's profile. This is checked once such a space binds orgs. An
  isolated space naming no profile uses the default login by design (D-59) and is not reported.

Membership comes from `gh api user/orgs`. Access as an outside collaborator is not detected, and a
token without `read:org` lists only public memberships.

Every other caller sees only how many problems there are.

**Which login each space uses** (SP-074.3, `server.spaceLogins`, `logins.go`). For every space of
the applied policy, orchd reports its gh login (the default or its `gh_config_dir`) and its Claude
Code configuration (the user's own or its `claude_config_dir`), each with a state: logged in, not
logged in, refused (a profile directory `CheckProfileDir` refuses; gh is not run) or unknown (the
check could not run). For gh it adds the account and where the token is stored (`tokenSource`
from `gh auth status --json hosts`: keyring, file, or a variable name). For Claude Code it runs
`claude auth status --json` with `CLAUDE_CONFIG_DIR` set only for a space's own folder, and
without `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or `CLAUDE_CODE_OAUTH_TOKEN`, so an
environment credential cannot answer for the folder. It decodes only `loggedIn` and `authMethod`,
so the email and organization it prints are never kept. Answers are cached like the gh profile
checks (5 minutes, 1 after a failure). They reach the operator only, through `ListSpaces`'s
`logins`, which `orch status` and Orch.app show.

**Claude Code folder per space** (SP-046, D-59; `Resolved.ClaudeConfigDir`). A space may name
`claude_config_dir`, a Claude Code configuration folder. Only `orch run <space> -- claude` uses
it, as the session's `CLAUDE_CONFIG_DIR`, so its sessions have that folder's own login and
transcripts; a space naming none runs with the user's own configuration. Other spaces' sessions
are denied the folder ([install.md](install.md#per-space-harness-settings)). orch never writes
or reads inside it.

**Validation** (`spaces.Validate`) reports every problem with a rule name: an unknown kind; a
bad or reserved name (`quarantine`, `shared`); a GitHub org that is not a well-formed GitHub login;
a GitHub org, web prefix (scheme and host compared
case-insensitively) or chat tenant (`slack:<id>`, `teams:<id>`) bound to two spaces; the same
root bound twice; a nested root less strict than a root above it (public < private < isolated);
a root of one isolated space inside another's; an isolated space reading an isolated one; a
private space reading an isolated one; a public space reading anything; a read of a space that
does not exist; a terms file inside a git working tree (a `.git` in any parent, after resolving
symlinks); a `gh_config_dir` that is not absolute, is inside a git working tree (gh may keep a
token in it) or is shared by two spaces (one profile is one account); a `claude_config_dir`
with the same faults (it holds a Claude Code login and transcripts); a policy over the size
limits; and a
`public_root_sessions` that is not a private or isolated space; and a bad `public_paths` entry
(below). Roots, terms files and
`gh_config_dir` and `claude_config_dir` may start with `~` and must otherwise be absolute. Roots are compared case-insensitively, since macOS
volumes usually are. With `public_root_sessions` unset, session content from under a public root
goes to the only private space when there is exactly one, else to quarantine.

**Public paths** (`public_paths`, SP-076, D-65; `Resolved.PublicPathFor`). A public space lists
the local folders under its roots whose content may be public; all other local content under a
public root is quarantined ([memory.md](memory.md#placement)). Nothing on disk, such as a `.git`,
counts as a declaration. An entry may start with `~` and must otherwise be absolute, and is
matched case-insensitively against the folder a file is in. Three forms only:

- an exact folder, `~/code/x`: the files directly in it, not those in its subfolders;
- `*` within one folder name, `~/code/*/docs` or `~/code/oss-*`: one folder name (Go's
  `path.Match`);
- `**` as a whole folder name, any depth including none: `~/code/x/**` is everything under
  `~/code/x`, and `~/code/**/docs` every `docs` folder below `~/code`.

There is no `!` negation and no other glob or gitignore syntax: an entry with `!`, `?`, `[`,
`]`, `{`, `}` or `\`, with `**` inside a longer name, or that is not a clean path (`.`, `..`,
`//`, a trailing `/`) is refused (`bad_public_path`), as is `public_paths` on a space that is not
public. The deepest root holding the folder an entry's literal names lead to (before any
wildcard) must be one of the space's own roots (`public_path_outside_root`): an entry may not sit
outside them, above them, or inside a deeper root of another space, which decides first. A
private or isolated root nested inside a `**` pattern keeps its own space. Entries are checked
when a draft is saved and at apply, and an apply that fails leaves the applied policy enforced.
These problems name the space and the entry's position, never the entry, so an error cannot echo
a term a folder name holds. `orch spaces why <path>` (alias `explain`) shows the rule:
`public_path` and, to the operator only, the pattern that matched; or `no_public_proof` saying no
pattern matched. A folder is explained for the files directly in it. `orch spaces diff` shows
entries added and removed. The app does not edit `public_paths` yet (SP-053). A listed folder
is trusted as written: any session can write files into it, or create a new folder a `*` or `**`
pattern matches, and the sources refresh indexes them into public (accepted risk RC-404), so list
only folders whose contents you would publish.

**Terms files** (`terms_file`, spaces SP-044): one term per line, a literal term or
`re:<regex>` (Go RE2 syntax). Matching is case-insensitive and on word boundaries (a term is not
found inside a longer word). Blank lines and lines starting with `#` are skipped. A file is
rejected when it is over 256 KiB, not UTF-8, holds over 4,096 terms or a line over 512 bytes,
an empty pattern, an invalid regular expression, a pattern that matches empty text or one that
defines a group named `termb` or `terma`; errors name the line number, never the term. A term is
named elsewhere only by its space, line number and the first 12 hex digits of its line's
HMAC-SHA256, keyed with the SHA-256 of the whole file. How the terms are enforced is in
[hooks.md](hooks.md#outbound-terms). How strictly commands are read is a fixed baseline of named
rules ([hooks.md](hooks.md#outbound-baseline), D-60); there is no setting for it yet. SP-075 will
let the operator switch baseline rules by id and add stricter ones. The whole check is switched
with the `terms` layer below, which orchd also writes into the guard's snapshot for the hook
(RC-116).

**Reads** (`Resolved.CanRead`): a space reads itself, every public space and the spaces it lists.
Lists do not chain, and nothing reads quarantine. With no policy applied, or one with no spaces,
everything is in one implicit `shared` space and orch behaves as it did before spaces.

**Enforcement switches** (SP-070; `spaces_enforcement` and `spaces_enforcement_log`, migration
`0029`). Like the policy, the switches are not settings in `config_values`: `SetConfig` and orch
settings cannot change them, and the tool hooks they cover cannot be turned off through orch
settings either (D-40). Each layer (`memory`, `guard`, `terms`, `sandbox`, `hooks`) is switched
only by the operator, with `SetEnforcement` (`orch spaces enforce <layer> <state> [--for 15m]`),
optionally for a time after which it reverts. The defaults are memory on, guard
`block-other-jobs`, terms on, sandbox on and hooks on. Every change is logged with time and
caller; while a layer is off, `orch status` and every session's start digest say so. See
[daemon.md](daemon.md#enforcement-switches).

**Outbound rules** (SP-075; `spaces_terms_rules`, migration `0030`). The outbound terms check's
baseline rules can each be switched off, and stricter rules added, by the operator only; like the
switches they are not settings, every change is logged with time and caller, and a rule's value
never leaves `orch.db` and the guard snapshot (INV-07). Switching the `terms` layer off turns the
whole check off, rules included. See [hooks.md](hooks.md#outbound-rules) and
[daemon.md](daemon.md#outbound-rules).

## Schema

`config.Fields` (`internal/config/schema.go`) describes every key: its section, label, help, type, range or
options, and two flags. It is what `GetConfig` serves and the app's Settings pane is built from. A test fails
when a default has no field.

- **Sensitive:** a key that changes what agents may run or where orch keeps its data. `SetConfig` refuses it
  without `confirm_sensitive`, and the app asks first. orch's own maintenance writes and `orch config reset`
  do not ask: the operator ran the command.
- **Live:** a key each short-lived process reads for itself (`hooks.auto_rag.*`, read by orch-hook on every
  prompt). Every other key takes effect when orchd restarts: `Restart` restarts subsystems in place, without
  reading config again.

`GetConfig` compares each key against a snapshot from orchd's start, so it reports what is waiting for a
restart however the value was changed, and a value set back stops waiting. Each field also says whether it is
`overridden` (set away from its default).

## Queue limits

`max_concurrent` and `max_active` are held to `config.MaxParallelTasks` and `config.MaxActiveTasks` (both 50)
on every route: `ApplyStore` refuses more, and `Unmarshal` clamps an imported or hand-edited value and logs it, raising `max_active` to at least `max_concurrent`. `max_active: 0` used to mean no cap and now means
50. The worker runs only when `queue.worker.enabled` is true and `max_concurrent` is above 0
(`QueueWorkerSettings.Runs`); `max_concurrent: 0` used to mean 1 and now means off. Setting it above 0 through
`ApplyStore` also turns `enabled` on and raises `max_active` if needed, so the app offers the one number.
