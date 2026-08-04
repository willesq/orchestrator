// Package mcpserver exposes orchd's RAG and queue over the Model Context Protocol
// (stdio). Each MCP tool is a thin translator onto an orchd gRPC call, so the same
// functionality is available identically to Claude Code, Codex, and Gemini — MCP is
// the cross-harness common denominator. The servers are spawned per-session by the
// harness and dial the long-lived orchd socket.
package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
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
		TopK:       int32(getInt(req, "top_k", 5)),
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

// NewRAG builds the RAG MCP server (tools: rag_search, rag_ingest_text).
func NewRAG(client orchdv1.OrchdClient) *server.MCPServer {
	s := server.NewMCPServer("orch-rag", version.Version)
	rt := ragTools{client: client}
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
	body := req.GetString("body", "")
	if body == "" {
		return mcp.NewToolResultError("body is required"), nil
	}
	resp, err := q.client.Dispatch(ctx, &orchdv1.DispatchRequest{
		Agent: req.GetString("agent", "dev"),
		Body:  body,
	})
	if err != nil {
		return mcp.NewToolResultErrorFromErr("dispatch failed", err), nil
	}
	return mcp.NewToolResultText("dispatched " + resp.GetInstanceId()), nil
}

// NewQueue builds the queue MCP server (tools: queue_dispatch).
func NewQueue(client orchdv1.OrchdClient) *server.MCPServer {
	s := server.NewMCPServer("orch-queue", version.Version)
	qt := queueTools{client: client}
	s.AddTool(mcp.NewTool("queue_dispatch",
		mcp.WithDescription("Dispatch a durable agent task to the orchestrator queue"),
		mcp.WithString("body", mcp.Required(), mcp.Description("task description")),
		mcp.WithString("agent", mcp.Description("target agent (default dev)")),
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

func getInt(req mcp.CallToolRequest, key string, def int) int {
	if v, ok := req.GetArguments()[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return def
}
