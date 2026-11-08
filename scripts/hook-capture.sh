#!/usr/bin/env bash
# Record real harness hook payloads into a scratch directory (spaces plan SP-001).
#
# The logging hook goes into project-local settings of a scratch git project, never
# into ~/.claude, ~/.codex or ~/.gemini. Each event becomes one redacted JSON line in
# <dir>/payloads/<harness>.jsonl, tagged with the current scenario label.
#
#   hook-capture.sh setup    <dir>          build the logger, create <dir>/project and <dir>/outside
#   hook-capture.sh scenario <dir> <label>  label the events that follow
#   hook-capture.sh launch   <dir>          print the commands that start each harness
#   hook-capture.sh summary  <dir> [harness] one row per event: scenario, event, cwd, tool
#   hook-capture.sh clean    <dir>          remove a directory this script created
#
# A PreToolUse call whose input contains HOOK_CAPTURE_DENY is denied, to test deny.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
MARKER=".hook-capture"

# Claude Code events worth recording. WorktreeCreate/WorktreeRemove are left out:
# a hook on them replaces the harness's own worktree handling.
CLAUDE_EVENTS=(SessionStart UserPromptSubmit PreToolUse PostToolUse PostToolUseFailure CwdChanged
  DirectoryAdded SubagentStart SubagentStop PreCompact PostCompact Stop SessionEnd)
# Codex 0.166 events with a payload of interest; it has no CwdChanged.
CODEX_EVENTS=(SessionStart UserPromptSubmit PreToolUse PostToolUse SubagentStart SubagentStop
  PreCompact PostCompact Stop SessionEnd)

die() { echo "hook-capture: $*" >&2; exit 1; }

# abs_dir prints an absolute path without creating it, resolving symlinks in its
# nearest existing ancestor so a link into a harness home cannot hide the target.
abs_dir() {
  [ -n "${1:-}" ] || die "missing <dir>"
  local p rest=""
  case "$1" in /*) p="$1" ;; *) p="$PWD/$1" ;; esac
  p="${p%/}"
  # Reason: '..' after a missing directory survives as text and would slip past
  # the prefix checks in refuse_real_home, then resolve inside a harness home.
  case "$p/" in */../* | */./*) die "use a path without '.' or '..' parts: $1" ;; esac
  while [ ! -d "$p" ]; do
    rest="/$(basename "$p")$rest"
    p="$(dirname "$p")"
  done
  echo "$(cd "$p" && pwd -P)$rest"
}

# refuse_real_home stops the script from writing anywhere a real harness or orch
# reads its configuration.
refuse_real_home() {
  local d="$1" home
  home="$(cd "$HOME" && pwd -P)"
  [ "$d" != "$home" ] || die "refusing to use the home directory itself"
  for p in .claude .codex .gemini .config/orch; do
    case "$d/" in "$home/$p/"*) die "refusing to write under ~/$p" ;; esac
  done
}

# hooks_json prints a hooks file that runs the logger for each named event.
hooks_json() {
  local bin="$1" out="$2" harness="$3"; shift 3
  printf '%s\n' "$@" | jq -R . | jq -s --arg bin "$bin" --arg out "$out" --arg h "$harness" '
    {hooks: (map({key: ., value: [{hooks: [{type: "command",
      command: ("\($bin | @sh) \($out | @sh) \($h) \(.)"), timeout: 10}]}]}) | from_entries)}'
}

cmd_setup() {
  local dir; dir="$(abs_dir "${1:-}")"
  refuse_real_home "$dir"
  umask 077
  mkdir -p "$dir"
  if [ -n "$(ls -A "$dir")" ] && [ ! -f "$dir/$MARKER" ]; then
    die "$dir is not empty and was not created by this script"
  fi
  command -v jq >/dev/null || die "jq is required"
  touch "$dir/$MARKER"
  mkdir -p "$dir/bin" "$dir/payloads" "$dir/project/sub" "$dir/outside"
  (cd "$REPO" && go build -o "$dir/bin/hook-capture" ./internal/hook/capture/cmd/hook-capture)

  local p="$dir/project"
  if [ ! -d "$p/.git" ]; then
    git -C "$p" init -q
    echo "scratch project for hook-capture" > "$p/README.md"
    echo "inside" > "$p/sub/file.txt"
    git -C "$p" add README.md sub/file.txt
    git -C "$p" -c user.name=hook-capture -c user.email=hook-capture@example.invalid \
      -c commit.gpgsign=false commit -q -m init
  fi
  echo "outside" > "$dir/outside/file.txt"

  mkdir -p "$p/.claude" "$p/.codex"
  hooks_json "$dir/bin/hook-capture" "$dir/payloads" claude-code "${CLAUDE_EVENTS[@]}" \
    > "$p/.claude/settings.local.json"
  hooks_json "$dir/bin/hook-capture" "$dir/payloads" codex "${CODEX_EVENTS[@]}" \
    > "$p/.codex/hooks.json"
  printf '.claude/\n.codex/\n' > "$p/.git/info/exclude"
  echo "ready: $dir"
  cmd_launch "$dir"
}

cmd_scenario() {
  local dir; dir="$(abs_dir "${1:-}")"
  [ -f "$dir/$MARKER" ] || die "$dir was not set up by this script"
  [ -n "${2:-}" ] || die "missing <label>"
  printf '%s\n' "$2" > "$dir/payloads/scenario"
  echo "scenario: $2"
}

cmd_launch() {
  local dir p o; dir="$(abs_dir "${1:-}")"
  p="$(printf %q "$dir/project")"; o="$(printf %q "$dir/outside")"
  cat <<EOF
Claude Code (project-local hooks in .claude/settings.local.json):
  cd $p && claude
  cd $p && CLAUDE_BASH_MAINTAIN_PROJECT_WORKING_DIR=1 claude
Codex (project-local hooks in .codex/hooks.json). The trust bypass applies to this
invocation only; never use it outside the scratch project:
  cd $p && codex --dangerously-bypass-hook-trust
Outside directory for scenarios: $o
EOF
}

cmd_summary() {
  local dir; dir="$(abs_dir "${1:-}")"
  local files=("$dir"/payloads/*.jsonl)
  [ -z "${2:-}" ] || files=("$dir/payloads/$2.jsonl")
  [ -e "${files[0]}" ] || die "no payloads recorded in $dir/payloads"
  jq -r '[.harness, (.scenario // "-"), .event, (.payload.cwd // "-"), .hook_cwd,
      (.payload.tool_name // "-"),
      ((.payload.tool_input // {}) | tostring | .[0:80])] | @tsv' "${files[@]}"
}

cmd_clean() {
  local dir; dir="$(abs_dir "${1:-}")"
  [ -d "$dir" ] || die "$dir does not exist"
  refuse_real_home "$dir"
  [ -f "$dir/$MARKER" ] || die "$dir was not created by this script; not removing"
  rm -rf "$dir"
  echo "removed $dir"
}

case "${1:-}" in
  setup | scenario | launch | summary | clean) c="$1"; shift; "cmd_$c" "$@" ;;
  *) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
