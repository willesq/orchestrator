---
title: "Project awareness and exploration"
group: Engineering practice
order: 10
description: "Learn the project's goals, conventions and tests before changing it."
---

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
