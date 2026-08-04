#!/usr/bin/env python3
"""Aggregate a Go coverage profile into per-file statement coverage.

Usage: go test -coverpkg=./... -coverprofile=cover.out ./...
       python3 scripts/cover_by_file.py cover.out [threshold]

Exits non-zero if any non-excluded file is below the threshold (default 80).
"""
import sys
from collections import defaultdict

EXCLUDE_SUFFIXES = (
    ".pb.go",          # generated protobuf
    "_grpc.pb.go",     # generated grpc
)


def is_excluded(fname: str) -> bool:
    """Exclude generated code and the thin cmd/*/main.go entrypoints.

    Entrypoint files contain only `func main`, which wires signal handling / os.Args
    and calls into a tested internal package; they are conventionally not unit-tested.
    All real logic lives in internal/* packages, which are held to the threshold.
    """
    if fname.endswith(EXCLUDE_SUFFIXES):
        return True
    if "/cmd/" in fname and fname.endswith("/main.go"):
        return True
    return False


def main() -> int:
    path = sys.argv[1] if len(sys.argv) > 1 else "cover.out"
    threshold = float(sys.argv[2]) if len(sys.argv) > 2 else 80.0

    covered = defaultdict(int)
    total = defaultdict(int)
    with open(path) as fh:
        lines = fh.read().splitlines()
    for line in lines[1:]:  # skip "mode:" header
        # form: file.go:start.col,end.col numstmts count
        loc, numstmts, count = line.rsplit(" ", 2)
        fname = loc.split(":")[0]
        n = int(numstmts)
        total[fname] += n
        if int(count) > 0:
            covered[fname] += n

    worst = []
    for fname in sorted(total):
        if is_excluded(fname):
            continue
        pct = 100.0 * covered[fname] / total[fname] if total[fname] else 100.0
        flag = "" if pct >= threshold else "  <-- BELOW"
        print(f"{pct:6.1f}%  {fname}{flag}")
        if pct < threshold:
            worst.append((fname, pct))

    print()
    if worst:
        print(f"{len(worst)} file(s) below {threshold:.0f}%:")
        for fname, pct in worst:
            print(f"  {pct:6.1f}%  {fname}")
        return 1
    print(f"All files >= {threshold:.0f}%")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
