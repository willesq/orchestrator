# CLI

`orch` is a cobra command tree (`internal/cli`). Commands that need orchd dial its socket; those
marked *operator-only* refuse when `ORCH_TASK_ID` is set (inside a queued task).

| Command | Purpose |
|---|---|
| `version`, `ping` | build version; orchd reachability |
| `status [--json]` | whole-system report; works with orchd down; always exits 0 |
| `check` | pass/fail preconditions: ONNX Runtime dylib, embedding model, `gh auth status`, orchd ping |
| `config get\|set\|list` | settings ([config.md](config.md)) |
| `daemon status\|start\|stop\|restart` | orchd via `brew services` or launchd |
| `restart [orchd\|subsystems\|<unit>] [--list]` | bounce orchd, or restart units in place |
| `install [--apply] [--target] [--home] [--bin-dir]` | wire harnesses ([install.md](install.md)) |
| `mcp rag\|queue` | stdio MCP servers |
| `ingest text --source-uri <uri> [--type] [--scope] [--tag]` | store stdin |
| `search <query> [--top-k] [--scope] [--type] [--tag] [--mode] [--since] [--until]` | search memory |
| `rag sources [--type] [--scope] [--prefix] [--limit] [--since] [--until]` | list stored sources |
| `rag forget <uri> --yes` | delete one source |
| `rag eval [add …]` | retrieval golden set and scoring |
| `rag migrate-hybrid [--target] [--apply] [--rollback] [--drop-backup]` | layout migration (*operator-only*) |
| `rag backfill-sessions [--apply]` | date older sessions (*operator-only*) |
| `source add\|list\|remove\|refresh`, `repo add\|list` | cadence-refreshed sources |
| `docs add\|list\|owners list\|status\|sync\|refresh\|remove` | GitHub owners' markdown |
| `dispatch <body> [--agent] [--cwd] [--kind] [--model]` | queue a task (refused inside a task) |
| `tasks [--status] [--agent] [--limit] [--stats]`, `task <id> [--rate]` | task ledger; rating refused inside a task |
| `model fetch\|refresh\|list\|enable\|disable\|tier`, `provider list\|enable\|disable` | embedding model and model registry (changes *operator-only*) |
| `routing show\|evaluate\|apply\|rollback\|set\|clear` | routing table (changes *operator-only*) |
| `qdrant status\|cutover\|migrate` | vector store engine |
| `prs [--blocked\|--ready\|--needs-me] [--json]` | cached PR status, including PRs awaiting your review (never calls `gh`); `--needs-me` lists what needs you, most urgent first |

## MCP tools

| Server | Tool | Parameters |
|---|---|---|
| `rag` | `rag_search` | query, top_k (5), scope, source_type, since, until |
| `rag` | `rag_ingest_text` | text, source_uri, source_type (`manual`), scope; same URI replaces |
| `rag` | `rag_list_sources` | source_type, scope, uri_prefix, limit (50), since, until |
| `rag` | `rag_delete_source` | source_uri, confirm (must be true) |
| `queue` | `queue_dispatch` | body, agent (`dev`), cwd, task_kind, model; refused inside a task |
| `queue` | `queue_list`, `queue_read` | status, agent, limit (20); id |
| `queue` | `model_recommend` | task_kind |

Numeric arguments are clamped (negative → default, above 10000 → 10000); orchd still rejects a
search `top_k` above 100. Results are plain text;
search hits read `N. [score] <uri> (<type>) · <when>` followed by the chunk text.
