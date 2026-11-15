"""Tests for planfiles.py, migrate.py and plantotals.py (SP-080.1 a1).

Run: python3 -m unittest discover -s docs/spaces/tools
"""

from __future__ import annotations

import contextlib
import copy
import io
import json
import os
import shutil
import tempfile
import unittest

import migrate
import planfiles
import plantotals

SPACES = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))


def fixture_plan() -> dict:
    """A small single-file (pre-SP-080) plan with reconvene items out of numeric order."""

    def feature(fid, status, evidence, **kw):
        f = {
            "id": fid, "phase": "P1", "priority": 1, "title": fid, "summary": "s", "why": "w",
            "depends_on": [], "touches": [], "docs_to_update": [],
            "checkpoints": [
                {"id": fid + ".1", "title": "one", "acceptance": [{"id": "a1", "check": "c"}], "evidence": evidence},
                {"id": fid + ".2", "title": "two", "acceptance": [{"id": "a1", "check": "c"}, {"id": "a2", "check": "d"}], "evidence": []},
            ],
            "status": status, "passes": status == "done", "verified_sha": None, "verified_at": None,
            "blocked_reason": None, "attempts": 0, "criteria_sha256": "x", "branch": None, "pr": None,
        }
        f.update(kw)
        return f

    def rc(n, status="open"):
        return {"id": f"RC-{n:02d}", "kind": "follow-up", "topic": "t", "detail": "d",
                "source": "https://github.com/o/r/pull/1", "raised": "2026-11-15", "affects": ["SP-001"], "status": status}

    return {
        "plan_id": "p", "title": "Plan", "updated": "2026-11-15",
        "decisions": {"confirmed_by_user": [], "open": [], "reconvene": [rc(1), rc(120), rc(3, "resolved"), rc(2)]},
        "features": [
            feature("SP-001", "done", ["a1: test passes"], passes=True, verified_sha="abc1234", verified_at="2026-11-15T00:00:00Z"),
            feature("SP-002", "in_progress", ["a1: half"], attempts=2, branch="feat/sp-002-x", pr="https://github.com/o/r/pull/2"),
            feature("SP-003", "todo", []),
        ],
        "changelog": [{"date": "2026-11-15", "change": "start"}],
        "status_values": {"todo": "", "in_progress": "", "blocked": "", "done": "", "superseded": ""},
    }


class TempDir(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="sp-080-")
        self.addCleanup(shutil.rmtree, self.dir)

    def write_plan(self, plan):
        with open(os.path.join(self.dir, planfiles.PLAN_NAME), "w", encoding="utf-8") as fh:
            fh.write(planfiles.dumps(plan))


class CombineSplitTest(unittest.TestCase):
    def test_round_trip_is_exact(self):
        plan = fixture_plan()
        parts = planfiles.split(plan)
        self.assertFalse(planfiles.is_combined(parts[0]))
        self.assertEqual(planfiles.dumps(planfiles.combine(*parts)), planfiles.dumps(plan))

    def test_combined_plan_combines_to_itself(self):
        plan = fixture_plan()
        self.assertTrue(planfiles.is_combined(plan))
        self.assertEqual(planfiles.combine(plan, {}, {}), plan)

    def test_missing_state_file_means_todo(self):
        plan, states, rcs, order = planfiles.split(fixture_plan())
        del states["SP-003"]
        f = planfiles.combine(plan, states, rcs, order)["features"][2]
        self.assertEqual((f["status"], f["attempts"], f["passes"]), ("todo", 0, False))
        self.assertEqual([c["evidence"] for c in f["checkpoints"]], [[], []])

    def test_new_items_follow_by_number_without_editing_order(self):
        plan, states, rcs, order = planfiles.split(fixture_plan())
        for n in (1551, 1500, 99):
            rcs[f"RC-{n}"] = dict(rcs["RC-01"], id=f"RC-{n}")
        ids = [r["id"] for r in planfiles.combine(plan, states, rcs, order)["decisions"]["reconvene"]]
        self.assertEqual(ids, ["RC-01", "RC-120", "RC-03", "RC-02", "RC-99", "RC-1500", "RC-1551"])
        ids = [r["id"] for r in planfiles.combine(plan, states, rcs, None)["decisions"]["reconvene"]]
        self.assertEqual(ids[:3], ["RC-01", "RC-02", "RC-03"])

    def test_value_in_both_places_is_refused(self):
        _, states, _, _ = planfiles.split(fixture_plan())
        with self.assertRaisesRegex(planfiles.PlanError, "SP-001: status"):
            planfiles.combine(fixture_plan(), {"SP-001": states["SP-001"]}, {})
        plan, states, rcs, order = planfiles.split(fixture_plan())
        plan["features"][0]["checkpoints"][0]["evidence"] = ["stale"]
        with self.assertRaisesRegex(planfiles.PlanError, "SP-001.1: evidence"):
            planfiles.combine(plan, states, rcs, order)
        with self.assertRaisesRegex(planfiles.PlanError, "RC-01"):
            planfiles.combine(fixture_plan(), {}, {"RC-01": rcs["RC-01"]})

    def test_stray_files_are_refused(self):
        plan, states, rcs, order = planfiles.split(fixture_plan())
        states["SP-001"]["evidence"]["SP-001.9"] = ["x"]
        with self.assertRaisesRegex(planfiles.PlanError, "unknown checkpoints SP-001.9"):
            planfiles.combine(plan, states, rcs, order)
        plan, states, rcs, order = planfiles.split(fixture_plan())
        states["SP-404"] = dict(states["SP-003"], id="SP-404")
        with self.assertRaisesRegex(planfiles.PlanError, "SP-404"):
            planfiles.combine(plan, states, rcs, order)

    def test_duplicate_reconvene_ids_are_refused(self):
        plan = fixture_plan()
        plan["decisions"]["reconvene"].append(copy.deepcopy(plan["decisions"]["reconvene"][0]))
        with self.assertRaisesRegex(planfiles.PlanError, "duplicate"):
            planfiles.split(plan)

    def test_write_refuses_ids_that_are_not_plain(self):
        plan, states, rcs, order = planfiles.split(fixture_plan())
        states["../SP-001"] = states.pop("SP-001")
        with self.assertRaisesRegex(planfiles.PlanError, "not usable as file names"):
            planfiles.write_split(tempfile.gettempdir(), plan, states, rcs, order)

    def test_check(self):
        plan, states, rcs, _ = planfiles.split(fixture_plan())
        self.assertEqual(planfiles.check(plan, states, rcs), [])
        states["SP-002"]["status"] = "doing"
        states["SP-002"]["note"] = "x"
        del rcs["RC-02"]["topic"]
        problems = planfiles.check(fixture_plan(), states, rcs)
        self.assertEqual(len(problems), 5, problems)  # embedded state, status, field, topic, clash


class MigrateTest(TempDir):
    def test_migrates_once_and_reproduces_the_plan(self):
        plan = fixture_plan()
        self.write_plan(plan)
        self.assertEqual(migrate.migrate(self.dir, dry_run=True), "would write 3 state files, 4 reconvene files")
        self.assertEqual(migrate.migrate(self.dir), "wrote 3 state files, 4 reconvene files")
        self.assertEqual(migrate.migrate(self.dir), "already migrated: nothing to do")
        left, states, rcs, order = planfiles.load_dir(self.dir)
        self.assertFalse(planfiles.is_combined(left))
        for f in left["features"]:
            self.assertFalse(set(planfiles.STATE_FIELDS) & set(f))
            self.assertFalse(any("evidence" in c for c in f["checkpoints"]))
        self.assertNotIn("reconvene", left["decisions"])
        self.assertEqual(order, ["RC-01", "RC-120", "RC-03", "RC-02"])
        self.assertEqual(states["SP-002"]["evidence"], {"SP-002.1": ["a1: half"], "SP-002.2": []})
        self.assertEqual(planfiles.dumps(planfiles.load_combined(self.dir)), planfiles.dumps(plan))
        self.assertEqual(planfiles.check(left, states, rcs), [])

    def test_refuses_a_half_migrated_directory(self):
        plan = fixture_plan()
        self.write_plan(plan)
        os.makedirs(os.path.join(self.dir, "reconvene"))
        with open(os.path.join(self.dir, "reconvene", "RC-1500.json"), "w") as fh:
            json.dump(dict(plan["decisions"]["reconvene"][0], id="RC-1500"), fh)
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(migrate.main(["--dir", self.dir]), 1)
        self.assertIn("convert_branch.py", err.getvalue())

    def test_refuses_a_plan_that_does_not_round_trip(self):
        plan = fixture_plan()
        del plan["features"][0]["branch"]
        self.write_plan(plan)
        with self.assertRaisesRegex(planfiles.PlanError, "round-trip"):
            migrate.migrate(self.dir)

    def test_refuses_to_write_through_symlinks(self):
        self.write_plan(fixture_plan())
        migrate.migrate(self.dir)
        plan, states, rcs, order = planfiles.load_dir(self.dir)
        target = os.path.join(self.dir, "outside.txt")
        with open(target, "w") as fh:
            fh.write("untouched")
        link = os.path.join(self.dir, "state", "SP-002.json")
        os.remove(link)
        os.symlink(target, link)
        states["SP-002"]["status"] = "blocked"
        with self.assertRaisesRegex(planfiles.PlanError, "symlink"):
            planfiles.write_split(self.dir, plan, states, rcs, order)
        with open(target) as fh:
            self.assertEqual(fh.read(), "untouched")
        # A planted link at a predictable temp name is not written through either.
        os.remove(link)
        os.symlink(target, link + ".tmp")
        planfiles.write_split(self.dir, plan, states, rcs, order)
        with open(target) as fh:
            self.assertEqual(fh.read(), "untouched")
        self.assertFalse(os.path.islink(link))
        self.assertEqual(planfiles.load_dir(self.dir)[1]["SP-002"]["status"], "blocked")
        self.assertEqual([n for n in os.listdir(os.path.join(self.dir, "state")) if n.startswith(".plan-")], [])

    def test_load_dir_checks_file_names(self):
        self.write_plan(fixture_plan())
        migrate.migrate(self.dir)
        os.rename(os.path.join(self.dir, "state", "SP-001.json"), os.path.join(self.dir, "state", "SP-009.json"))
        with self.assertRaisesRegex(planfiles.PlanError, "does not match"):
            planfiles.load_dir(self.dir)

    def test_cli_and_prune(self):
        self.write_plan(fixture_plan())
        with contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(migrate.main(["--dir", self.dir]), 0)
        self.assertIn("wrote 3 state files", out.getvalue())
        plan, states, rcs, order = planfiles.load_dir(self.dir)
        del rcs["RC-03"]
        with open(os.path.join(self.dir, "reconvene", "README.txt"), "w") as fh:
            fh.write("kept")
        planfiles.write_split(self.dir, plan, states, rcs, order)
        self.assertEqual(sorted(os.listdir(os.path.join(self.dir, "reconvene"))),
                         ["RC-01.json", "RC-02.json", "RC-120.json", "README.txt", "order.json"])


class TotalsTest(TempDir):
    def test_totals_match_before_and_after_migration(self):
        plan = fixture_plan()
        self.write_plan(plan)
        before = plantotals.totals(planfiles.load_combined(self.dir))
        self.assertEqual(before["status"], {"blocked": 0, "done": 1, "in_progress": 1, "superseded": 0, "todo": 1})
        self.assertEqual((before["checkpoints"], before["checkpoints_with_evidence"]), (6, 3))
        self.assertEqual((before["acceptance"], before["acceptance_covered"]), (9, 4))
        self.assertEqual(before["reconvene"], {"total": 4, "open": 3, "resolved": 1})
        migrate.migrate(self.dir)
        self.assertEqual(plantotals.totals(planfiles.load_combined(self.dir)), before)

    def test_cli(self):
        self.write_plan(fixture_plan())
        out_path = os.path.join(self.dir, "combined.json")
        with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
            # The single-file plan fails --check: its state is still embedded.
            self.assertEqual(plantotals.main(["--dir", self.dir, "--check"]), 1)
            migrate.migrate(self.dir)
            self.assertEqual(plantotals.main(["--dir", self.dir, "--check", "--json", "--combined", out_path]), 0)
            self.assertEqual(plantotals.main(["--dir", self.dir]), 0)
        self.assertIn("still embeds", err.getvalue())
        self.assertIn("features 3: blocked 0, done 1", out.getvalue())
        with open(out_path, encoding="utf-8") as fh:
            self.assertEqual(json.load(fh), fixture_plan())
        os.rename(os.path.join(self.dir, "state", "SP-001.json"), os.path.join(self.dir, "state", "SP-009.json"))
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(plantotals.main(["--dir", self.dir]), 1)


class RepositoryFilesTest(TempDir):
    """The committed docs/spaces files: consistent, and a fresh migration of their
    combined view reproduces them."""

    def test_committed_files_round_trip(self):
        plan, states, rcs, order = planfiles.load_dir(SPACES)
        combined = planfiles.combine(plan, states, rcs, order)
        if planfiles.is_combined(plan):
            self.skipTest("this checkout is not migrated yet")
        self.assertEqual(planfiles.check(plan, states, rcs), [])
        self.write_plan(combined)
        migrate.migrate(self.dir)
        self.assertEqual(planfiles.load_dir(self.dir)[0], plan)
        self.assertEqual(planfiles.load_combined(self.dir), combined)


if __name__ == "__main__":
    unittest.main()
