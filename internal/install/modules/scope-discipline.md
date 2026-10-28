---
title: "Scope discipline for mid-task requests"
group: Engineering practice
order: 110
description: "Park unrelated requests as their own scope (park / ask / interrupt)."
---

### 🧭 Scope discipline — mid-implementation requests

The shared policy says record unrelated work separately. This is the specific
protocol, including the vocabulary a project can set to override it.

A new request arriving mid-implementation of an uncommitted scope does **NOT** get spliced
into the in-flight work by default. Interleaving is how code gets **orphaned** — half-done,
untested, forgotten.

1. **Is it genuinely part of the current scope?** "Add input validation" / "make sure it's
   secure" while building that same endpoint → fold it in, that IS the current scope. A
   distinct concern → new scope, steps 2–3.
2. **Park it** in whatever backlog that project uses, with today's date, a short spec, the
   source (who asked / why), and a "**parked as its own scope**" note. Then finish the current
   scope (implement → verify → commit) before picking it up.
3. **Briefly acknowledge** that you parked it and will address it next as its own scope.

**Per-project policy** may be set in the project's planning or instruction file (e.g. a
"Mid-task requests:" line). Honor it. Recognized values:
- `park` *(default)* — park + brief mention; finish current scope first.
- `ask` — pause and ask "implement now, or park for later?" before deciding.
- `interrupt` — immediate context-switch (rare; only when explicitly set).

With no policy set, default to `park`, but **ask** when intent is genuinely ambiguous.
**Never** start a second scope on top of an unfinished, uncommitted first scope without
finishing/committing it or explicit user direction to switch.
