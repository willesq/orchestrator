---
title: "Structure, style, and design"
group: Engineering practice
order: 20
description: "Focused modules, simple designs, and why-comments."
---

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
