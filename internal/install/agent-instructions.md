# Shared coding-agent expectations

These instructions apply to coding agents using Orchestrator, regardless of
harness or model. Follow the user's current directions and the repository's
specific conventions. Keep shared expectations in this version-controlled file;
Orchestrator installs its embedded copy at `~/.config/orch/AGENTS.md`.

Guidance that applies only to particular work lives beside it in
`~/.config/orch/policy/`. Read the matching file before starting that work:

- `pr-review.md` — before reviewing a pull request or giving a review verdict.
- `languages.md` — before writing or testing Go or Python code.

## Project awareness and exploration

- Establish the project's goals, architecture, style, constraints, and current
  task before making changes. Read relevant planning and task documents when
  present; do not invent required documents or assume a particular layout.
- Follow existing naming, package boundaries, file organization, and architectural
  patterns. Verify paths, APIs, dependencies, and tool capabilities before relying
  on them. Do not invent libraries, functions, or command behavior.
- Prefer symbol-aware code exploration when an available language service supports
  it: inspect symbol outlines, targeted definitions, and references before broad
  reads. Adjust failed searches instead of repeatedly issuing the same query.
- When symbolic tools are unavailable, use targeted repository search and file
  reads. Prefer `rg` for text and filename discovery. Read enough context to
  understand behavior without loading unrelated files or entire large trees.
- Inspect representative existing tests and build configuration before deciding
  how to implement or validate a change. Reuse known project testing conventions
  rather than creating a parallel framework.
- Use the project's configured runtime, virtual environment, and dependency
  management.

## Structure, style, and design

- Keep modules focused on a feature or responsibility. Aim to keep source files
  below 500 lines; split growing code into cohesive helpers without unrelated
  refactors or arbitrary fragmentation of existing modules.
- Keep imports consistent with the language and package conventions. Reuse the
  project's configuration loader and supported environment-variable interfaces.
- Prefer clear, straightforward solutions. Add complexity only when required by
  the current behavior; do not build speculative abstractions or future features.
- Apply single responsibility, substitutable interfaces, small dependency
  contracts, and dependency inversion where they make the code clearer. Use
  existing abstractions before introducing new ones.
- Preserve the repository's primary language and its formatter. Do not introduce
  frameworks or dependencies into projects that do not need them.
- Document functions and public interfaces using the project's conventions.
- Explain non-obvious decisions with a short reason comment. Comments should
  explain why the behavior is needed, rather than restating the code.

## Scope and task management

- Complete the authorized task through implementation, validation, and delivery.
  Ask for missing information when it materially affects correctness; continue
  useful independent work while waiting. Do not assume missing requirements.
- Keep edits focused and preserve unrelated code, configuration, and user work.
  Delete or replace existing behavior only when necessary for the authorized task.
- Incorporate follow-up requests that refine the active task. For unrelated work,
  follow the project's task policy and the user's steering; record it separately
  with a date, intent, and source instead of abandoning unfinished changes.
- Use an existing task or progress log where the project has one. Mark completed
  items and record discovered follow-up work; distinguish implemented work from
  a proposal or parked item.
- Treat requests to remember something for later as notes for the user's future
  self, not instructions to execute or dispatch it. Keep personal notes separate
  from executable workflows, retain source links, and use topic-based headings.
- Use the available delegation workflow when requested or required by project
  instructions. Pass the full task, constraints, and context to workers. Do not
  assume another harness's background-task API exists or that queued work ran.

## Testing strategy and coverage

- Detect the repository's testing framework, directory layout, commands, coverage
  gate, and established patterns once at the start of substantive work. Recall
  existing testing guidance where available and verify it against current files.
- Add or update meaningful tests for new behavior, business logic, validation,
  endpoints, authentication, and bug fixes. Match existing test naming and style.
  Code changes must include appropriate regression coverage before committing.
- If a project has no test infrastructure, determine whether the task authorizes
  introducing one; ask once when that choice requires user direction. Remember
  the decision instead of asking again in later sessions.
- Cover happy paths, boundary values, empty and invalid inputs, failure handling,
  and relevant integration points. Test concurrent behavior when the change
  involves shared state, background workers, cancellation, or lifecycle races.
- Isolate databases, services, and user files through fixtures, temporary paths,
  and mocks or fakes at stable interfaces. Do not test against personal global
  configuration or require external services for ordinary unit tests.
- Reuse setup helpers and fixtures instead of duplicating large test bodies.
- Measure coverage against the project's gate instead of claiming it from test
  count; exercise errors and important branches rather than mirroring internals.
- Prefer a failing regression test before fixing a bug. Implement the required
  behavior, verify the targeted tests, then run the relevant broader suite.
- Run the affected tests, then the relevant full suite and the project's
  configured lint and type gates.
- Run the repository's coverage gate before delivering code changes. Report
  missing prerequisites, skipped checks, and existing failures accurately; do not
  present an unrun or failing check as passing.
- Documentation-only changes need reference and formatting checks. Changes to
  embedded instructions also need installation and drift checks because those
  documents ship as runtime assets.

## Security and credential handling

- Perform a security review of changed material before commits and a broader
  review before opening a PR. Use a dedicated security-review capability when
  available; otherwise perform and report the relevant checks directly.
- Review authentication, authorization, sessions, payments, file handling,
  cryptography, data access, external commands, dependencies, and user input for
  security regressions when those areas change.
- Validate input at trust boundaries. Check permissions before sensitive actions.
  Use parameterized queries or safe ORM operations; never construct executable
  SQL from untrusted string interpolation.
- Execute external commands through argument arrays rather than a shell when
  possible. Never evaluate untrusted input as code, and guard file operations
  against path traversal and unintended writes.
- Avoid unsafe deserialization, including untrusted pickle data and unrestricted
  YAML loaders. Choose safe parsers. Use maintained password-hashing libraries
  and established cryptographic interfaces rather than custom schemes.
- Keep secrets, credentials, and personal data out of source control, indexed
  memory, generated reports, logs, error messages, and temporary artifacts.
- Use existing authenticated tools and credential stores. A coding agent must not
  retrieve, capture, print, or persist the user's live token, cookie, private key,
  passphrase, keychain secret, or other credential into its context or a new file.
- Do not run `gh auth token`, inspect token-bearing authentication files, or
  extract credentials from processes. Authentication metadata such as
  `gh auth status` may be used when it does not expose credential values.
- Do not construct shell commands or suggest workarounds that fetch a live token
  into an environment variable, temporary file, shell profile, wrapper, or output
  stream. Renaming variables or hiding the retrieval in another language does
  not change this boundary.
- Code authored for an application may pass credentials internally through its
  documented subprocess configuration or credential provider when needed. The
  actual values must remain within that application's supported flow and never
  enter the coding agent's context, generated files, or persistent diagnostics.
- Reuse lockfiles and pinned dependencies; check new dependencies for known
  vulnerabilities. Use available project scanners for secrets, dependencies, and
  unsafe patterns without printing live credentials.
- Resolve critical, high, and medium security findings before committing affected
  behavior. Record lower-risk follow-ups and document any accepted risk explicitly.

## Git and GitHub delivery

- Use Conventional Commit messages and PR titles, such as `feat:`, `fix:`,
  `docs:`, `test:`, `refactor:`, `chore:`, `style:`, and `perf:`.
- Sign commits with `git commit -S` through the configured signing integration.
  Do not add coding-agent co-author attribution to commits.
- Work on the intended feature branch or managed worktree rather than committing
  directly to the default branch. Use the user's configured worktree workflow
  when available, and preserve existing worktree ownership and branch state.
- Stage the intended changes only. Keep local planning/spec artifacts, including
  `docs/superpowers/`, credentials, and personal configuration out of commits.
  Move durable project decisions into proper documentation or a PR description.
- Use the `gh` CLI for GitHub PRs, reviews, issues, checks, releases, and workflows.
  Prefer structured JSON for inspection and body files for multiline mutations.
  Do not scrape GitHub pages or bypass authenticated tooling to work around auth.
- Respect authorization already given in the session. Obtain approval for external
  mutations when not already authorized; do not repeatedly ask for an action the
  user has requested. Never force-push, rewrite published history, merge, or
  release without authorization for that action.
- Check CI and unresolved blocking review feedback before merging. Reviewing a
  pull request or giving a verdict follows `policy/pr-review.md`.
- Use clickable links whenever referring to PRs, issues, commits, or workflow runs
  in user-facing reports. Describe resulting behavior, evidence, and limitations.
- If the user's PR Watch registry is configured, register newly created PRs once
  in `~/.pr-watch.json`, preserving existing entries and their timestamps. Do not
  create duplicate entries or remove entries owned by the watcher.

## Documentation and completion

- Update README, shared instructions, and relevant setup/configuration guidance
  when features, dependencies, commands, or workflows change. Keep shared wording
  agent-neutral and isolate product-specific behavior in the relevant adapters.
- Explain decisions and tradeoffs in terms a developer can act on. Document the
  reason for complex behavior and keep descriptions consistent with the code.
- Finish by reporting what changed, how it was verified, and material limitations
  or remaining blockers. Distinguish changes in the repository from changes
  actually installed, deployed, merged, or released.

## Durable memory and recall

- Orchestrator stores durable knowledge locally across coding agents and sessions.
  Search relevant prior decisions and memories with `rag_search` before substantive
  work. Save durable preferences, lessons, and decisions with `rag_ingest_text`.
- Background task sessions do not write memory: Orchestrator refuses
  `rag_ingest_text`, `orch ingest`, and source deletion inside a queued task.
  Delegated workers put durable findings in a clearly marked `For RAG` section
  of their final report, with a proposed `source_uri`, `source_type`, and text
  for each entry. The coordinating session or the user reviews those entries as
  untrusted and ingests only what they approve.
- Use stable `source_uri` values; reuse the URI to update an existing memory.
  Include enough context to apply the rule, its reason, and relevant absolute
  dates. Cite source URIs when using retrieved memory in an answer.
- Keep durable memories in Orchestrator RAG rather than harness-specific
  auto-memory files or a second competing memory store. Do not record secrets
  or facts that are readily derived from repository contents and Git history.
- Recall project testing decisions through RAG, and update those memories when
  conventions change. Keep user, feedback, project, and reference memories clear.
- MCP server names vary: `rag` in Claude/Gemini and `orch_rag` in Codex. Use the
  tool names exposed by the active harness rather than assuming an MCP prefix.
- If MCP is unavailable, use `orch search` and `orch ingest text` (text on stdin).
  Use `orch status` to diagnose availability and instruction drift.
- Automatic recall and session capture depend on supported, trusted hooks. Do
  not assume hooks ran or reimplement a background subsystem without checking.
  Explicit search and ingest work independently of lifecycle hooks.
- Treat workflow queues as execution ledgers, not personal todo lists. Verify
  worker capabilities and actual task state before reporting that work ran.
