"""Status and reconvene totals of the spaces plan, from either layout (SP-080).

Usage:
  python3 docs/spaces/tools/plantotals.py                 # the working tree's docs/spaces
  python3 docs/spaces/tools/plantotals.py --ref origin/main
  python3 docs/spaces/tools/plantotals.py --json --combined OUT.json --check

The totals count what docs/spaces/index.html shows: features by status, checkpoints
with evidence (a done feature counts all of its checkpoints, as the reader does),
acceptance checks covered, open decisions and reconvene items by status.
--combined writes the single-file view, which the reader also accepts.
--check fails when the split files are inconsistent (state left in the plan file,
an unknown status, a state file for a missing feature, evidence for a missing
checkpoint, a malformed reconvene item).

Standard library only.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from collections import Counter

import planfiles
import plangit

DEFAULT_DIR = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))


def totals(combined: dict) -> dict:
    feats = combined["features"]
    status = Counter(f["status"] for f in feats)
    for k in combined.get("status_values", {}):
        status.setdefault(k, 0)
    cps = cps_done = acc = acc_done = 0
    for f in feats:
        for c in f["checkpoints"]:
            cps += 1
            acc += len(c["acceptance"])
            if f["status"] == "done" or c.get("evidence"):
                cps_done += 1
                acc_done += len(c["acceptance"])
    rc = Counter(r.get("status") for r in combined["decisions"].get("reconvene", []))
    return {
        "features": len(feats),
        "status": dict(sorted(status.items())),
        "checkpoints": cps,
        "checkpoints_with_evidence": cps_done,
        "acceptance": acc,
        "acceptance_covered": acc_done,
        "open_decisions": len(combined["decisions"].get("open", [])),
        "reconvene": {"total": sum(rc.values()), **dict(sorted(rc.items()))},
    }


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    src = ap.add_mutually_exclusive_group()
    src.add_argument("--dir", default=DEFAULT_DIR, help="a docs/spaces directory (default: this script's parent)")
    src.add_argument("--ref", help="read the files committed at this git ref instead")
    ap.add_argument("--repo", default=".", help="repository for --ref (default: the current directory)")
    ap.add_argument("--json", action="store_true", help="print the totals as JSON")
    ap.add_argument("--combined", metavar="OUT", help="also write the combined single-file plan to OUT")
    ap.add_argument("--check", action="store_true", help="exit 1 if the split files are inconsistent")
    args = ap.parse_args(argv)
    try:
        parts = plangit.read_ref(args.repo, args.ref) if args.ref else planfiles.load_dir(args.dir)
        combined = planfiles.combine(*parts)
    except planfiles.PlanError as e:
        print(f"plantotals: {e}", file=sys.stderr)
        return 1
    if args.combined:
        with open(args.combined, "w", encoding="utf-8") as fh:
            fh.write(planfiles.dumps(combined))
    t = totals(combined)
    if args.json:
        print(json.dumps(t, indent=2))
    else:
        print(f"features {t['features']}: " + ", ".join(f"{k} {v}" for k, v in t["status"].items()))
        print(f"checkpoints with evidence {t['checkpoints_with_evidence']} / {t['checkpoints']}; "
              f"acceptance covered {t['acceptance_covered']} / {t['acceptance']}")
        print(f"open decisions {t['open_decisions']}; reconvene " +
              ", ".join(f"{k} {v}" for k, v in t["reconvene"].items()))
    if args.check:
        problems = planfiles.check(*parts[:3])
        if problems:
            print("\n".join("check: " + p for p in problems), file=sys.stderr)
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
