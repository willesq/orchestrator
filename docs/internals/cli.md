# CLI

`orch` is a cobra command tree (`internal/cli`). Commands that need orchd dial its socket; those
marked *operator-only* refuse when `ORCH_TASK_ID` is set (inside a queued task).

| Command | Purpose |
|---|---|
| `version`, `ping` | build version; orchd reachability |
| `status [--json]` | whole-system report, including the `safety-net` line; works with orchd down; always exits 0 |
| `check` | pass/fail preconditions: ONNX Runtime dylib, embedding model, `gh auth status`, orchd ping |
| `config get\|set\|list` | settings ([config.md](config.md)) |
| `daemon status\|start\|stop\|restart` | orchd via `brew services` or launchd |
| `restart [orchd\|subsystems\|<unit>] [--list]` | bounce orchd, or restart units in place |
| `install [--apply] [--target] [--home] [--bin-dir]` | wire harnesses, and for Claude Code the pinned cc-safety-net and its guard rules ([install.md](install.md)) |
| `mcp rag\|queue` | stdio MCP servers |
| `ingest text --source-uri <uri> [--type] [--scope] [--tag]` | store stdin |
| `search <query> [--top-k] [--scope] [--type] [--tag] [--require-tag] [--uri] [--mode] [--ranking] [--per-source] [--since] [--until]` | search memory |
| `rag sources [--type] [--scope] [--prefix] [--limit] [--since] [--until]` | list stored sources |
| `rag forget <uri> --yes` | delete one source |
| `rag eval [--set] [--top-k] [--save] [--compare] [--mode] [--ranking] [--per-source] [--section] [--split] [--inject-format] [--cases] [--json]`, `rag eval add <query> <uri>… [--span] [--section]`, `rag eval seed` | retrieval golden set, scoring and paired-bootstrap comparison ([memory.md](memory.md#retrieval-evaluation)) |
| `rag why [prompt] [--cwd] [--json]` | the prompt from stdin when omitted or `-`; replay prompt context for a prompt: whether it would search, each candidate's score and verdict, the exact block; changes nothing ([hooks.md](hooks.md#prompt-context)) |
| `rag backfill-sessions [--all [--summaries]] [--apply]` | store undated sessions again, or with `--all` every transcript still on disk, without harness text, and with `--summaries` their summaries too ([memory.md](memory.md#transcripts-and-harness-text)) |
| `rag dedupe [--apply]` | delete path-keyed copies identical to a synced doc of a registered source (*operator-only*) |
| `rag reindex-keywords [--apply]` | rebuild keyword vectors for the current tokenizer, in place (*operator-only*) |
| `rag migrate-hybrid [--target] [--apply] [--rollback] [--drop-backup]` | layout migration (*operator-only*) |
| `rag backfill-sessions [--apply]` | date older sessions (*operator-only*) |
| `source add\|list\|remove\|refresh [--confirm-prune]`, `repo add\|list` | cadence-refreshed sources; `--confirm-prune` lets the next refresh of one source prune past the cap (through orchd, operator-only) |
| `docs add\|list\|owners list\|status\|sync [--confirm-prune]\|refresh\|remove` | GitHub owners' markdown |
| `dispatch <body> [--agent] [--cwd] [--kind] [--model]` | queue a task (refused inside a task) |
| `tasks [--status] [--agent] [--limit] [--stats]`, `task <id> [--rate]` | task ledger; rating refused inside a task |
| `model fetch\|refresh\|list\|enable\|disable\|tier`, `provider list\|enable\|disable` | embedding model and model registry (changes *operator-only*) |
| `routing show\|evaluate\|apply\|rollback\|set\|clear` | routing table (changes *operator-only*) |
| `qdrant status\|cutover\|migrate` | vector store engine |
| `github scopes [--refresh] [--json]`, `github enable <owner> [--all-items\|--everything-in <repos>] [--include\|--exclude <repos>]`, `github disable <owner>`, `github teams [--refresh]`, `github repos <owner>` | choose which GitHub accounts and orgs orch monitors (enable/disable *operator-only*) |
| `settings list [--provider] [--kind] [--json]`, `settings enable\|disable <kind> <id> [--provider]` | managed Claude Code hooks, permissions and plugins and cc-safety-net rules: list with state and origin, turn one on or off (enable/disable *operator-only*; [settings.md](settings.md)) |
| `prs [--blocked\|--ready\|--needs-me] [--json]` | cached status of PRs in your enabled GitHub scopes (never calls `gh`); `--needs-me` lists what needs you, most urgent first |
| `prs watches` | the PRs orchd watches for review: state, what each may post, verdict and note (`ListPRReviews`) |
| `prs stop <pr-url> [--forget]` | stop watching a PR, or also delete its verdict (`StopPRReview`); says how to cancel a review still running (*operator-only*) |

## MCP tools

| Server | Tool | Parameters |
|---|---|---|
| `rag` | `rag_search` | query, top_k (5), scope, source_type, tags (any), all_tags, source_uri, mode, per_source, since, until |
| `rag` | `rag_ingest_text` | text, source_uri, source_type (`manual`), scope; same URI replaces |
| `rag` | `rag_list_sources` | source_type, scope, uri_prefix, limit (50), since, until |
| `rag` | `rag_delete_source` | source_uri, confirm (must be true) |
| `queue` | `queue_dispatch` | body, agent (`dev`), cwd, task_kind, model; refused inside a task |
| `queue` | `queue_list`, `queue_read` | status, agent, limit (20); id |
| `queue` | `model_recommend` | task_kind |

Numeric arguments are clamped (negative → default, above 10000 → 10000); orchd still rejects a
search `top_k` above 100. Results are plain text;
search hits read `N. [score] "<uri>" ("<type>") · <when> · <trust label>` (URI and type quoted)
followed by the chunk text fenced as quoted data, in every tier, with instruction-like text
flagged ([hooks.md](hooks.md#prompt-context)). `rag_list_sources` quotes every stored field (URI,
type, scope, tags), and so does `orch search` (URI and type, with the text on one line).
