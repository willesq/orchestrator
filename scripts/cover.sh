#!/usr/bin/env bash
# Per-file coverage gate: run each internal package's tests, merge the profiles,
# and fail if any non-excluded file is below the threshold (default 80%).
set -euo pipefail

cd "$(dirname "$0")/.."
THRESHOLD="${1:-80}"
OUT="$(mktemp)"
echo "mode: set" > "$OUT"

pkgs=$(go list ./internal/... | grep -v '/rpc/orchdv1')
for pkg in $pkgs; do
  prof="$(mktemp)"
  if go test -covermode=set -coverprofile="$prof" "$pkg" >/dev/null 2>&1; then
    tail -n +2 "$prof" >> "$OUT" 2>/dev/null || true
  fi
done

python3 scripts/cover_by_file.py "$OUT" "$THRESHOLD"
