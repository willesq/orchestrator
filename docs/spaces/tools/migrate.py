"""One-shot migration of the spaces plan to per-feature files (SP-080, D-80).

Usage: python3 docs/spaces/tools/migrate.py [--dir docs/spaces] [--dry-run]

Moves every feature's status, passes, verified_sha, verified_at, blocked_reason,
attempts, branch, pr and checkpoint evidence into state/<ID>.json, every reconvene
item into reconvene/<RC-id>.json, and the items' current order into
reconvene/order.json. The plan file keeps definitions, criteria, decisions,
changelog and procedure. Before writing it checks that combining the new files
reproduces the current plan byte for byte, and checks it again after writing.
A plan that is already split is left alone, so running it twice changes nothing.

Standard library only.
"""

from __future__ import annotations

import argparse
import os
import sys

import planfiles

DEFAULT_DIR = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))


def migrate(spaces_dir: str, dry_run: bool = False) -> str:
    """Migrate spaces_dir in place and return a one-line summary."""
    plan, states, rcs, order = planfiles.load_dir(spaces_dir)
    if not planfiles.is_combined(plan):
        return "already migrated: nothing to do"
    if states or rcs:
        raise planfiles.PlanError("the plan embeds state but state or reconvene files already exist; use convert_branch.py")
    # The plan as loaded, in its own key order: what the new files must combine back to.
    before = planfiles.dumps(plan)
    new_plan, new_states, new_rcs, new_order = planfiles.split(plan)
    if planfiles.dumps(planfiles.combine(new_plan, new_states, new_rcs, new_order)) != before:
        raise planfiles.PlanError("split and combine do not round-trip; nothing written")
    summary = f"{len(new_states)} state files, {len(new_rcs)} reconvene files"
    if dry_run:
        return "would write " + summary
    planfiles.write_split(spaces_dir, new_plan, new_states, new_rcs, new_order)
    if planfiles.dumps(planfiles.load_combined(spaces_dir)) != before:
        raise planfiles.PlanError("the written files do not combine to the original plan")
    return "wrote " + summary


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    ap.add_argument("--dir", default=DEFAULT_DIR, help="the docs/spaces directory (default: this script's parent)")
    ap.add_argument("--dry-run", action="store_true", help="check and report without writing")
    args = ap.parse_args(argv)
    try:
        print(migrate(args.dir, args.dry_run))
    except planfiles.PlanError as e:
        print(f"migrate: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
