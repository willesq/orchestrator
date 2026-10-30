# Repository guidance

Before editing, read and follow the complete shared coding expectations in
`internal/install/agent-instructions.md` and the instruction modules in
`internal/install/modules/`. That version-controlled policy covers
project exploration, modularity, testing and coverage, scope discipline, security
and credentials, Git/GitHub delivery, reviews, documentation, and durable memory.
These repository conventions supplement that policy. Keep shared expectations
in the policy source so every installed coding agent receives the same rules.

## Purpose

Orchestrator is a local-first memory and workflow layer for coding agents. It
keeps durable knowledge available across sessions and harnesses through a shared
daemon, retrieval tools, lifecycle hooks, and common instructions. Preserve that
cross-agent intent when changing behavior; use agent-neutral wording for shared
policies and isolate harness-specific behavior in adapters.

## Architecture and consistency

- The three entry points are `orch` (CLI), `orchd` (daemon), and `orch-hook`
  (harness hooks). Keep clients thin and long-lived services owned by the daemon.
- Follow existing package boundaries under `internal/`. Read
  `docs/ARCHITECTURE.md` for the structure, `docs/internals/` for the area you
  change, and the guides it links for current commands; verify design notes
  against the implementation and update `docs/internals/` with the behavior.
- Keep CLI behavior, MCP tools, hooks, and documentation consistent when changing
  a shared capability. Check each affected harness rather than assuming identical
  configuration formats, event payloads, or instruction-loading behavior.
- Read and follow `internal/install/agent-instructions.md`, the version-controlled
  cross-agent policy embedded in the CLI and installed at
  `~/.config/orch/AGENTS.md`, and its sections in `internal/install/modules/`
  (one file per intent, each toggled per machine in orch's settings and
  installed to `~/.config/orch/modules/`). Its on-demand sections live in
  `internal/install/policy/` and install to `~/.config/orch/policy/`. Keep policy edits in that source; personal rules stay
  in harness entry files. This file holds repository editing conventions. Use
  native imports or explicit read directives in harness entry files.
- Preserve local-first operation, durable memory, stable source URIs, and
  recoverable workflow state. Keep secrets out of indexed content and logs.

## Making changes

- Keep edits focused, follow nearby Go conventions, and format changed Go files
  with `gofmt`. Reuse existing abstractions before adding new ones.
- Install changes must remain dry-run by default, idempotent, and backed up
  before modifying existing files. Preserve personal instructions and third-party
  configuration. Judge harness configuration drift by meaning where supported.
- Exercise install behavior with a temporary `--home`; avoid modifying the
  developer's global configuration while testing.
- Add meaningful regression tests for behavior changes, including preservation
  and repeat-operation cases when changing installation or lifecycle handling.
- Update user-facing documentation when commands, configuration, or supported
  behavior change. State limitations accurately rather than implying parity that
  has not been verified.
- Treat generated RPC files as generated artifacts. Change the `.proto` source
  and use `make proto` when the RPC interface changes.

## Validation and delivery

- For code changes, run relevant package tests, then `go test ./...` and
  `go vet ./...` before delivery. CI also runs `make cover`, which requires at
  least 80% coverage per eligible file, and builds with the `ORT` tag on macOS.
  CI runs only on pull requests to `main`, filtered by the paths each workflow
  checks; no checks run on `main` itself, so a change lands only through a PR
  whose checks passed. Push a branch when it is ready for a PR run. The one
  workflow on `main`, `cache-main.yml`, only refills the shared build caches
  when `go.mod`, `go.sum` or `macos/Package.resolved` change; caches are keyed
  `<kind>-<main|branch>-<runner>-<SHA of those files>`
  (`.github/actions/scoped-cache`), and releases restore none.
- Native builds use `make build` and require the tokenizers static library and
  ONNX Runtime described in `docs/INSTALL.md`. Pure-Go tests do not require those
  native dependencies. Report any checks that could not run.
- For documentation-only changes, check references and `git diff --check`;
  rerun code tests only when the changes affect executable behavior.
- Use Conventional Commit messages and sign commits with `git commit -S`.
- Describe the resulting behavior, validation, and material limitations in PRs
  and handoffs. Keep this guidance concise and update it as conventions change.
