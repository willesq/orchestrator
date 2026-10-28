---
title: "Personal todo list"
group: Personal workflow
order: 200
description: "\"Remember this\" goes to ~/notes/todos.md, never the dispatch queue."
---

### 📝 Personal todo list — `~/notes/todos.md`

The shared policy says keep "remember this" notes separate from executable workflows. This is
the file and the dispatch firewall.

A long-lived, **agnostic** notes file for items the user wants to come back to later — a
parking lot for their future self, not a queue for execution.

**Triggers:** "add this to my todos / list / notes", "track this for me", "i want to come back
to this", "park this", "remind me later", "save this as a todo / follow-up", "i'll do this
later", "not now but don't forget" — any context where they're collecting for their future self
rather than asking for action now.

**File rules:**
- File name and `##` headings stay **agnostic** — never named after a single PR / feature /
  ticket. The file lives forever; subsections come and go.
- Each `##` is a topic / source bucket. **Don't merge unrelated items under one heading.** Group
  only when items share genuine surface area (e.g. several flag bugs in the same tool).
- `- [ ]` for actionable items; plain paragraphs for reference-only context.
- Preserve per-item source links (PR URLs, comment anchors, tickets).
- Slot new items into a matching `##` if one fits, else add one. Don't reorganize unrelated
  existing sections unasked.

**Firewall vs. the orchestrator queue:**

| | Personal todos (`~/notes/todos.md`) | Orchestrator dispatch (`orch-dispatch`) |
|---|---|---|
| **Intent** | "Remember this for me — I'll do it later." | "Go do this work." |
| **Executor** | The user, eventually. | A specialist (`dev`/`ops`/`research`/`reviewer`) in background. |
| **Pickup** | Never auto-picked-up. Inert until opened. | Background workers; tracked in `/orch-status`. |
| **Trigger words** | track / park / remember / come back to / save for later | fix / build / investigate / review / dispatch / kick off / open a PR for |

**Hard rules:** do **NOT** call `mcp__queue__queue_dispatch`; do **NOT** invoke `orch-dispatch`,
the dispatcher subagent, or spawn a specialist; do **NOT** create an entry under
`~/.claude/orchestrator/queue/`. If intent is genuinely ambiguous ("add a task for X" with no
later/track signal), **ask once** before writing anywhere.

The user's nouns ("task", "todo", "follow-up") map to either store depending on whether they
want it **done** or **remembered**. Read the intent, not the noun.
