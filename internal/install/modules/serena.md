---
title: "Serena MCP is the primary code reader"
group: Code exploration
order: 100
description: "Read and explore code through Serena's symbol tools rather than whole-file reads."
---

### 🔍 Codebase exploration — Serena MCP is the primary code reader

The shared policy says "prefer symbol-aware exploration." On this machine that
means Serena, specifically:

- **ALWAYS use Serena MCP tools to read and explore code files** – using the standard `Read` tool for code files is WRONG.
  - `mcp__serena__get_symbols_overview` to understand a code file's structure (first step, always).
  - `mcp__serena__find_symbol` to read specific functions, classes, or methods by name (`include_body=True`).
  - `mcp__serena__search_for_pattern` to search for code patterns or text across the codebase.
  - `mcp__serena__find_referencing_symbols` to find where symbols are used.
- **Only use the `Read` tool for non-code files** (markdown, config, text, JSON, YAML).
- **If a Serena tool errors**, retry with a different Serena tool or adjust parameters (pattern search instead of symbol search, substring matching, broaden/narrow scope).
- **Never read entire code files** – overview first, then targeted `find_symbol` reads.
- **Token efficiency is critical** – symbolic reading saves thousands of tokens over full reads.

**Serena language support.** Supported: AL, Bash, C#, C/C++ *(reference-finding is flaky)*,
Clojure, Dart, Elixir *(needs NextLS + Elixir; no Windows)*, Elm *(needs Elm compiler)*,
Erlang *(needs beam + erlang_ls; experimental, may hang)*, Fortran *(needs `pip install fortls`)*,
Go *(needs gopls)*, Haskell *(auto-locates HLS via ghcup/stack/PATH; Stack + Cabal)*, Java,
JavaScript, Julia, Kotlin *(pre-alpha official LS)*, Lua, Markdown *(needs explicit
`--language markdown` at project-config generation)*, Nix *(needs nixd)*, Perl *(needs
Perl::LanguageServer)*, PHP *(Intelephense; `INTELEPHENSE_LICENSE_KEY` for premium)*, Python,
R *(needs the `languageserver` R package)*, Ruby *(ruby-lsp default; `ruby_solargraph` for the
old impl)*, Rust *(needs rustup → rust-analyzer)*, Scala *(Metals; some manual setup)*, Swift,
TypeScript, Zig *(needs ZLS)*.
