// Package mcpserver exposes orchd's RAG and queue over the Model Context Protocol
// (stdio). Each MCP tool is a thin translator onto an orchd gRPC call, so the same
// functionality is available identically to Claude Code, Codex, and Gemini — MCP is
// the cross-harness common denominator. The servers are spawned per-session by the
// harness and dial the long-lived orchd socket.
package mcpserver

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/taskfmt"
	"github.com/willesq/orchestrator/internal/version"
)

// ragTools binds the RAG MCP tools to an orchd client.
type ragTools struct{ client orchdv1.OrchdClient }

func (r ragTools) search(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	resp, err := r.client.Search(ctx, &orchdv1.SearchRequest{
		Query:      query,
		TopK:       getInt(req, "top_k", 5),
		Scope:      req.GetString("scope", ""),
		SourceType: req.GetString("source_type", ""),
	})
	if err != nil {
		return mcp.NewToolResultErrorFromErr("search failed", err), nil
	}
	return mcp.NewToolResultText(formatHits(resp.GetHits())), nil
}

func (r ragTools) ingest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	text := req.GetString("text", "")
	uri := req.GetString("source_uri", "")
	if text == "" || uri == "" {
		return mcp.NewToolResultError("text and source_uri are required"), nil
	}
	resp, err := r.client.Ingest(ctx, &orchdv1.IngestRequest{
		Text:       text,
		SourceUri:  uri,
		SourceType: req.GetString("source_type", "manual"),
		Scope:      req.GetString("scope", ""),
	})
	if err != nil {
		return mcp.NewToolResultErrorFromErr("ingest failed", err), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("ingested %d chunk(s) from %s", resp.GetChunks(), uri)), nil
}

func (r ragTools) listSources(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := r.client.ListSources(ctx, &orchdv1.ListSourcesRequest{
		SourceType: req.GetString("source_type", ""),
		Scope:      req.GetString("scope", ""),
		UriPrefix:  req.GetString("uri_prefix", ""),
		Limit:      getInt(req, "limit", 50),
	})
	if err != nil {
		return mcp.NewToolResultErrorFromErr("list sources failed", err), nil
	}
	return mcp.NewToolResultText(formatSources(resp)), nil
}

func (r ragTools) deleteSource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uri := req.GetString("source_uri", "")
	if uri == "" {
		return mcp.NewToolResultError("source_uri is required"), nil
	}
	// The model has to pass confirm explicitly. Deletion is permanent, and
	// content reaching this store includes untrusted text (web pages, PR bodies),
	// so a tool that deletes on a bare URI is one prompt injection away from
	// wiping memory. Requiring the flag makes the choice visible in the transcript.
	if !req.GetBool("confirm", false) {
		return mcp.NewToolResultError(
			"refusing to delete without confirm=true: this permanently removes every chunk " +
				"under " + uri + " and cannot be undone. Confirm with the user first."), nil
	}
	resp, err := r.client.DeleteSource(ctx, &orchdv1.DeleteSourceRequest{
		SourceUri: uri, Confirm: true,
	})
	if err != nil {
		return mcp.NewToolResultErrorFromErr("delete failed", err), nil
	}
	if resp.GetDeleted() == 0 {
		return mcp.NewToolResultText("no chunks matched " + uri + " — nothing deleted"), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("deleted %d chunk(s) from %s", resp.GetDeleted(), uri)), nil
}

// NewRAG builds the RAG MCP server (tools: rag_search, rag_ingest_text,
// rag_list_sources, rag_delete_source).
func NewRAG(client orchdv1.OrchdClient) *server.MCPServer {
	s := server.NewMCPServer("orch-rag", version.Version)
	rt := ragTools{client: client}
	s.AddTool(mcp.NewTool("rag_list_sources",
		mcp.WithDescription("List the distinct sources held in the RAG store, with chunk counts. "+
			"This is the store's inventory, unlike 'orch source list' which shows the refresh registry."),
		mcp.WithString("source_type", mcp.Description("filter by source type")),
		mcp.WithString("scope", mcp.Description("filter by scope")),
		mcp.WithString("uri_prefix", mcp.Description("filter to URIs with this prefix, e.g. session://")),
		mcp.WithNumber("limit", mcp.Description("max sources to return (default 50, 0 for all)")),
	), rt.listSources)
	s.AddTool(mcp.NewTool("rag_delete_source",
		mcp.WithDescription("Permanently delete every chunk belonging to one source URI. "+
			"Unrecoverable: there is no undo. Confirm with the user before calling, and only "+
			"for a URI they named."),
		mcp.WithString("source_uri", mcp.Required(), mcp.Description("the exact source URI to remove")),
		mcp.WithBoolean("confirm", mcp.Required(),
			mcp.Description("must be true; set it only after the user has asked for this exact URI to be deleted")),
	), rt.deleteSource)
	s.AddTool(mcp.NewTool("rag_search",
		mcp.WithDescription("Search the orchestrator RAG knowledge store"),
		mcp.WithString("query", mcp.Required(), mcp.Description("natural-language query")),
		mcp.WithNumber("top_k", mcp.Description("max results (default 5)")),
		mcp.WithString("scope", mcp.Description("filter by scope, e.g. work|personal|project:x")),
		mcp.WithString("source_type", mcp.Description("filter by source type")),
	), rt.search)
	s.AddTool(mcp.NewTool("rag_ingest_text",
		mcp.WithDescription("Ingest text into the orchestrator RAG store"),
		mcp.WithString("text", mcp.Required(), mcp.Description("content to ingest")),
		mcp.WithString("source_uri", mcp.Required(), mcp.Description("stable source URI")),
		mcp.WithString("source_type", mcp.Description("source type (default manual)")),
		mcp.WithString("scope", mcp.Description("scope tag")),
	), rt.ingest)
	return s
}

// queueTools binds the queue MCP tools to an orchd client.
type queueTools struct{ client orchdv1.OrchdClient }

func (q queueTools) dispatch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// A queued session runs with this server in its own MCP config. Refusing here
	// keeps one task from spawning more, which would otherwise fan out unbounded.
	if id := os.Getenv(agentrun.TaskIDEnv); id != "" {
		return mcp.NewToolResultError("queue tasks cannot dispatch further tasks (running as " + id + "); " +
			"do the work in this session or report BLOCKED"), nil
	}
	body := req.GetString("body", "")
	if body == "" {
		return mcp.NewToolResultError("body is required"), nil
	}
	cwd := req.GetString("cwd", "")
	if cwd == "" {
		// The harness starts this server in its session's working directory.
		cwd, _ = os.Getwd()
	}
	resp, err := q.client.Dispatch(ctx, &orchdv1.DispatchRequest{
		Agent: req.GetString("agent", "dev"),
		Body:  body,
		Cwd:   cwd,
	})
	if err != nil {
		return mcp.NewToolResultErrorFromErr("dispatch failed", err), nil
	}
	msg := "dispatched " + resp.GetInstanceId() + " (runs in " + cwd + ")"
	if !resp.GetWorkerEnabled() {
		msg += "\n" + workerDisabledNotice
	}
	return mcp.NewToolResultText(msg), nil
}

func (q queueTools) list(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := q.client.ListTasks(ctx, &orchdv1.ListTasksRequest{
		Status: req.GetString("status", ""),
		Agent:  req.GetString("agent", ""),
		Limit:  getInt(req, "limit", 20),
	})
	if err != nil {
		return mcp.NewToolResultErrorFromErr("list tasks failed", err), nil
	}
	tasks := resp.GetTasks()
	if len(tasks) == 0 {
		return mcp.NewToolResultText("no tasks recorded"), nil
	}
	var b strings.Builder
	for _, t := range tasks {
		fmt.Fprintf(&b, "%s  [%s] agent=%s  updated=%s\n    %s\n",
			t.GetId(), t.GetStatus(), t.GetAgent(), t.GetUpdatedAt(), firstLine(t.GetBody()))
	}
	return mcp.NewToolResultText(b.String()), nil
}

func (q queueTools) read(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := req.GetString("id", "")
	if id == "" {
		return mcp.NewToolResultError("id is required"), nil
	}
	resp, err := q.client.GetTask(ctx, &orchdv1.GetTaskRequest{Id: id})
	if err != nil {
		return mcp.NewToolResultErrorFromErr("read task failed", err), nil
	}
	if !resp.GetFound() {
		return mcp.NewToolResultText("no task with id " + id), nil
	}
	t := resp.GetTask()
	return mcp.NewToolResultText(fmt.Sprintf(
		"id: %s\nstatus: %s\nagent: %s\nupdated: %s\n%s\nbody:\n%s\n\nresult:\n%s\n",
		t.GetId(), t.GetStatus(), t.GetAgent(), t.GetUpdatedAt(), taskfmt.Outcome(t),
		t.GetBody(), orNone(t.GetResult()))), nil
}

// workerDisabledNotice explains a dispatch that will not run yet.
const workerDisabledNotice = "NOTE: the queue worker is disabled, so this task stays pending. Enable it with " +
	"`orch config set queue.worker.enabled true` and `orch restart`; pending tasks then run."

// maxArgInt bounds any caller-supplied count argument.
const maxArgInt = 10_000

// NewQueue builds the queue MCP server (tools: queue_dispatch, queue_list,
// queue_read).
func NewQueue(client orchdv1.OrchdClient) *server.MCPServer {
	s := server.NewMCPServer("orch-queue", version.Version)
	qt := queueTools{client: client}
	s.AddTool(mcp.NewTool("queue_list",
		mcp.WithDescription("List dispatched tasks, newest first"),
		mcp.WithString("status", mcp.Description("filter: pending | in_progress | complete | failed | blocked")),
		mcp.WithString("agent", mcp.Description("filter by target agent")),
		mcp.WithNumber("limit", mcp.Description("max tasks (default 20)")),
	), qt.list)
	s.AddTool(mcp.NewTool("queue_read",
		mcp.WithDescription("Read one dispatched task by ID"),
		mcp.WithString("id", mcp.Required(), mcp.Description("the task ID returned by queue_dispatch")),
	), qt.read)
	s.AddTool(mcp.NewTool("queue_dispatch",
		mcp.WithDescription("Dispatch a durable background task. When the queue worker is enabled, orchd runs it "+
			"as a headless Claude Code session (claude -p --agent <agent>) in cwd and records the agent's "+
			"final message as the result; check it with queue_read. The session cannot ask questions, so "+
			"the body must carry everything the agent needs."),
		mcp.WithString("body", mcp.Required(), mcp.Description("complete, self-contained task description")),
		mcp.WithString("agent", mcp.Description("target subagent (default dev)")),
		mcp.WithString("cwd", mcp.Description("absolute directory to run in (default: this session's working directory)")),
	), qt.dispatch)
	return s
}

// Serve runs an MCP server over stdio until the client disconnects.
func Serve(s *server.MCPServer) error {
	return server.ServeStdio(s)
}

func formatHits(hits []*orchdv1.SearchHit) string {
	if len(hits) == 0 {
		return "no results"
	}
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. [%.3f] %s (%s)\n   %s\n",
			i+1, h.GetScore(), h.GetSourceUri(), h.GetSourceType(), h.GetText())
	}
	return b.String()
}

func formatSources(resp *orchdv1.ListSourcesResponse) string {
	srcs := resp.GetSources()
	if len(srcs) == 0 {
		return "no sources indexed"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d source(s), %d chunk(s)",
		resp.GetTotalSources(), resp.GetTotalChunks())
	if int(resp.GetTotalSources()) > len(srcs) {
		fmt.Fprintf(&b, " — showing %d", len(srcs))
	}
	b.WriteString("\n\n")
	for _, s := range srcs {
		fmt.Fprintf(&b, "%6d  %-20s %s", s.GetChunks(), s.GetSourceType(), s.GetSourceUri())
		if s.GetScope() != "" {
			fmt.Fprintf(&b, "  scope=%s", s.GetScope())
		}
		if len(s.GetTags()) > 0 {
			fmt.Fprintf(&b, "  tags=%s", strings.Join(s.GetTags(), ","))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// firstLine trims a body down to one readable line for list output.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 100 {
		return s[:100] + "…"
	}
	return s
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none yet)"
	}
	return s
}

// getInt reads an integer argument, clamped to a sane range.
//
// The bound matters because callers narrow the result to int32 downstream: an
// out-of-range or NaN float64 converts to an implementation-defined int, which
// can wrap to a value that means something entirely different (0 reads as "no
// limit" on some paths). Clamping keeps a nonsense argument merely nonsensical.
func getInt(req mcp.CallToolRequest, key string, def int32) int32 {
	if v, ok := req.GetArguments()[key]; ok {
		switch n := v.(type) {
		case float64:
			if math.IsNaN(n) || n < 0 {
				return def
			}
			if n > maxArgInt {
				return maxArgInt
			}
			return int32(n)
		case int:
			if n < 0 {
				return def
			}
			return int32(min(n, maxArgInt))
		}
	}
	return def
}
