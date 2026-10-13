# Language conventions: Go and Python

Read this before writing or testing Go or Python code. It extends the shared
coding-agent expectations in `~/.config/orch/AGENTS.md`.

## Code

- In Go, use idiomatic packages, explicit error handling, and `gofmt`. In Python,
  follow PEP 8, use type hints, and follow the project's formatter, commonly Black.
- In Python projects that already use them, follow Pydantic validation, FastAPI
  endpoint conventions, and the established SQLAlchemy or SQLModel data layer.
  Do not introduce these dependencies into projects that do not need them.
- Python docstrings should explain purpose, arguments, and returns; Go comments
  should describe exported behavior and meaningful constraints.
- For Python, use the project's existing virtual environment rather than the
  system interpreter; do not assume a machine-specific environment name.

## Tests

- Prefer table-driven Go tests and pytest parametrization for related scenarios.
  Reuse setup helpers and fixtures instead of duplicating large test bodies.
- Python tests should use the existing `tests/` layout, pytest fixtures, scoped
  monkeypatching or mocks, exception assertions, and the configured async plugin
  where needed. Use project-supported coverage commands such as `pytest --cov`.
- Go tests belong in appropriate `*_test.go` files. Use `t.Run` for scenarios,
  `t.Helper` for assertions and setup, and `t.Cleanup` for teardown. Use interfaces
  for controllable external dependencies and project-supported build tags for
  integration tests. Add assertion libraries only when already appropriate.
- Aim for at least 90% coverage on new Python code and 80% on new Go code, subject
  to stricter project gates.
- For Go code changes, run the applicable tests, `go test ./...`, and
  `go vet ./...`; use `go test -race` for changed concurrent behavior. Run relevant
  integration tests when their prerequisites are available.
- For Python code changes, run affected tests, the full relevant suite, and
  coverage with the project's environment. Follow its configured lint/type gates.
