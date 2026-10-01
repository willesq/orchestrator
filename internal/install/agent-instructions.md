## Orchestrator memory

Orchestrator stores durable memory locally across coding agents and sessions.
Use the registered RAG MCP server's rag_search tool to recall relevant prior
decisions and memories before substantive work. Use rag_ingest_text to save
durable user preferences, lessons, and decisions with a stable source_uri.
When updating a memory, reuse its source_uri. Cite source URIs when using memory.
Keep durable memories in RAG rather than harness-specific auto-memory files.
Do not store secrets or facts readily derived from the repository.

MCP server names vary by harness: rag in Claude/Gemini, orch_rag in Codex.
If MCP is unavailable, use orch search and orch ingest text (text on stdin).
Use orch status to diagnose availability. Automatic recall and session capture
depend on the harness's supported hooks; do not assume they ran. Explicit search
and ingest work independently of lifecycle hooks.
