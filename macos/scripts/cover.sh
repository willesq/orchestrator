#!/usr/bin/env bash
# Per-file line-coverage gate for OrchKit, matching the Go gate's bar
# (default 80%). Generated gRPC code is excluded, as the Go gate excludes
# internal/rpc/orchdv1. The app target is SwiftUI views and is not gated.
set -euo pipefail

pkg="$(cd "$(dirname "$0")/.." && pwd)"
threshold="${1:-80}"

swift test --enable-code-coverage --package-path "$pkg" >/dev/null
profdata="$(dirname "$(swift test --package-path "$pkg" --show-codecov-path)")/default.profdata"
binary="$pkg/.build/debug/OrchPackageTests.xctest/Contents/MacOS/OrchPackageTests"

files=()
for f in "$pkg"/Sources/OrchKit/*.swift; do files+=("$f"); done

xcrun llvm-cov report "$binary" -instr-profile "$profdata" "${files[@]}" |
  awk -v min="$threshold" '
    /\.swift / {
      cover = $(NF-3); sub("%", "", cover)
      printf "%-28s %6s%%\n", $1, cover
      if (cover + 0 < min) { bad = bad " " $1 }
    }
    END {
      if (bad != "") { print "below " min "%:" bad; exit 1 }
    }'
