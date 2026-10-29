---
title: "Testing strategy and coverage"
group: Engineering practice
order: 40
description: "Regression tests, isolation, and running the coverage gate before delivery."
---

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
