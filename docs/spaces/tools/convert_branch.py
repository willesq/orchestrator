"""Convert a branch's plan-file edits into per-feature files while merging main (SP-080).

Usage, on a branch cut before the SP-080 migration:

    git merge origin/main          # stops with a conflict in docs/spaces/spaces-features.json
    python3 docs/spaces/tools/convert_branch.py
    git commit -S                  # the merge commit

It reads the plan files at three commits (the merge base, HEAD and MERGE_HEAD by
default), combines each into the single-file view, merges the three by meaning
(planmerge.py: features, checkpoints and reconvene items by id, appended lists as
appends) and writes the result in the split layout: the branch's status, evidence
and reconvene items land in state/<ID>.json and reconvene/<RC-id>.json, its other
plan edits (definitions, changelog) stay in spaces-features.json. It then stages
those paths. If both sides changed the same value differently it lists the paths,
writes nothing and exits 1. Run it again after resolving any other conflicts; it
only touches the plan files.

Outside a merge, give the commits: --base, --ours and --theirs (for example after a
merge commit: --base $(git merge-base HEAD^1 HEAD^2) --ours HEAD^1 --theirs HEAD^2).

Standard library only.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys

import planfiles
import plangit
import planmerge


def _side(repo, ref):
    plan, states, rcs, order = plangit.read_ref(repo, ref)
    return {
        "split": not planfiles.is_combined(plan),
        "combined": planfiles.combine(plan, states, rcs, order),
        "states": set(states),
        "order": order,
    }


def convert(repo: str, base: str, ours: str, theirs: str, dry_run: bool = False):
    """Merge the plan files of three commits into repo's working tree.

    Returns (conflicts, summary). Nothing is written when conflicts is non-empty.
    """
    b, o, t = (_side(repo, r) for r in (base, ours, theirs))
    merged, conflicts = planmerge.merge(b["combined"], o["combined"], t["combined"])
    if conflicts:
        return conflicts, "conflicting edits: nothing written"
    spaces = os.path.join(repo, plangit.SPACES)
    if not (o["split"] or t["split"]):
        # Neither side has migrated: keep the single-file layout, merged by meaning.
        if not dry_run:
            with open(os.path.join(spaces, planfiles.PLAN_NAME), "w", encoding="utf-8") as fh:
                fh.write(planfiles.dumps(merged))
        return [], "merged the single-file plan"
    plan, states, rcs, merged_order = planfiles.split(merged)
    if t["split"]:
        order = t["order"] if t["order"] is not None else o["order"]
    else:
        # The migrated branch is catching up with an unmigrated main: keep main's order,
        # then this branch's own items, as the migration would have.
        order = merged_order
    by_id = {f["id"]: f for f in plan["features"]}
    keep = {
        fid: s
        for fid, s in states.items()
        if fid in o["states"] or fid in t["states"] or s != planfiles.default_state(by_id[fid])
    }
    summary = f"{len(keep)} state files, {len(rcs)} reconvene items"
    if dry_run:
        return [], "would write " + summary
    planfiles.write_split(spaces, plan, keep, rcs, order)
    plangit.git(
        repo, "add", "-A", "--",
        f"{plangit.SPACES}/{planfiles.PLAN_NAME}",
        f"{plangit.SPACES}/{planfiles.STATE_DIR}",
        f"{plangit.SPACES}/{planfiles.RC_DIR}",
    )
    return [], "wrote and staged " + summary


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    ap.add_argument("--repo", default=".", help="the repository (default: the current directory)")
    ap.add_argument("--base", help="merge base commit (default: merge-base of --ours and --theirs)")
    ap.add_argument("--ours", help="this branch's commit (default: HEAD)")
    ap.add_argument("--theirs", help="the commit being merged in (default: MERGE_HEAD)")
    ap.add_argument("--dry-run", action="store_true", help="report without writing")
    args = ap.parse_args(argv)
    try:
        repo = plangit.git(args.repo, "rev-parse", "--show-toplevel").decode().strip()
        ours = plangit.resolve(repo, args.ours or "HEAD")
        if args.theirs is None:
            try:
                theirs = plangit.resolve(repo, "MERGE_HEAD")
            except planfiles.PlanError:
                print("convert_branch: no merge in progress. Run `git merge origin/main` first, "
                      "or give --theirs (and --ours, --base).", file=sys.stderr)
                return 2
        else:
            theirs = plangit.resolve(repo, args.theirs)
        if args.base:
            base = plangit.resolve(repo, args.base)
        else:
            base = plangit.git(repo, "merge-base", ours, theirs).decode().split()[0]
        conflicts, summary = convert(repo, base, ours, theirs, args.dry_run)
    except (planfiles.PlanError, subprocess.CalledProcessError, ValueError) as e:
        detail = getattr(e, "stderr", None)
        print(f"convert_branch: {(detail.decode().strip() if detail else e)}", file=sys.stderr)
        return 1
    if conflicts:
        print("Both sides changed these differently; edit one side and run again:", file=sys.stderr)
        print("\n".join("  " + c for c in conflicts), file=sys.stderr)
        return 1
    print(summary)
    return 0


if __name__ == "__main__":
    sys.exit(main())
