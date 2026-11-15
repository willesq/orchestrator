"""Git merge tests for the split plan layout (SP-080.1 a2 and a4).

Each test builds a throwaway repository from the real plan (its combined view), so
the merges run on the same file sizes and shapes lanes see. Git runs without the
user's global configuration, hooks or signing.

Run: python3 -m unittest discover -s docs/spaces/tools
"""

from __future__ import annotations

import contextlib
import io
import os
import shutil
import subprocess
import tempfile
import unittest

import convert_branch
import migrate
import planfiles
import planmerge
import plantotals

SPACES = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
REL = "docs/spaces"


class Repo(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="sp-080-git-")
        self.addCleanup(shutil.rmtree, self.root)
        empty = os.path.join(self.root, "gitconfig")
        open(empty, "w").close()
        self.env = dict(os.environ, GIT_CONFIG_GLOBAL=empty, GIT_CONFIG_NOSYSTEM="1",
                        GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@example.invalid",
                        GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@example.invalid")
        self.repo = os.path.join(self.root, "repo")
        self.spaces = os.path.join(self.repo, REL)
        os.makedirs(self.spaces)
        self.git("init", "-q", "-b", "main")
        self.git("config", "core.hooksPath", os.path.join(self.root, "no-hooks"))
        self.git("config", "commit.gpgsign", "false")
        self.plan = planfiles.load_combined(SPACES)
        self.write_plan(self.plan)
        self.commit("plan before SP-080")
        a, b, c = [f for f in self.plan["features"]][:3]
        self.fa, self.fb, self.fc = a["id"], b["id"], c["id"]
        # New reconvene numbers above every real one, so the test never collides with the plan.
        self.n = max(planfiles.rc_sort_key(r["id"])[0] for r in self.plan["decisions"]["reconvene"]) + 1

    def git(self, *args, check=True):
        r = subprocess.run(["git", "-C", self.repo, *args], env=self.env, capture_output=True, text=True)
        if check and r.returncode:
            self.fail(f"git {' '.join(args)}: {r.stdout}{r.stderr}")
        return r

    def commit(self, msg):
        self.git("add", "-A")
        self.git("commit", "-q", "-m", msg)

    def write_plan(self, plan):
        with open(os.path.join(self.spaces, planfiles.PLAN_NAME), "w", encoding="utf-8") as fh:
            fh.write(planfiles.dumps(plan))

    def combined(self):
        return planfiles.load_combined(self.spaces)

    def feature(self, combined, fid):
        return next(f for f in combined["features"] if f["id"] == fid)

    def rc(self, n, affects):
        return {"id": f"RC-{n}", "kind": "follow-up", "topic": f"item {n}", "detail": "d",
                "source": "https://github.com/o/r/pull/1", "raised": "2026-11-15",
                "affects": [affects], "status": "open"}

    def lane_edit_combined(self, fid, rc_n, status="in_progress"):
        """A pre-migration lane's edit: status, evidence and a reconvene item in the plan file."""
        plan = self.combined()
        f = self.feature(plan, fid)
        f.update(status=status, attempts=f["attempts"] + 1, branch=f"feat/{fid.lower()}-x")
        f["checkpoints"][0]["evidence"].append(f"a1: evidence from {fid}'s lane")
        plan["decisions"]["reconvene"].append(self.rc(rc_n, fid))
        plan["changelog"].append({"date": "2026-11-15", "change": f"{fid} lane note"})
        self.write_plan(plan)

    def lane_edit_split(self, fid, rc_n, status="in_progress"):
        """A migrated lane's edit: its own state file and a new reconvene file."""
        _, states, rcs, order = planfiles.load_dir(self.spaces)
        plan = planfiles.load_dir(self.spaces)[0]
        s = states[fid]
        s.update(status=status, attempts=s["attempts"] + 1, branch=f"feat/{fid.lower()}-x")
        first = next(iter(s["evidence"]))
        s["evidence"][first].append(f"a1: evidence from {fid}'s lane")
        rcs[f"RC-{rc_n}"] = self.rc(rc_n, fid)
        planfiles.write_split(self.spaces, plan, states, rcs, order)

    def merge(self, ref):
        return self.git("merge", "--no-edit", "-q", ref, check=False)

    def unmerged(self):
        return self.git("diff", "--name-only", "--diff-filter=U").stdout.split()


class ParallelLanesMergeTest(Repo):
    """a2: two branches that each change a different feature's state merge without conflict."""

    def test_split_layout_merges_cleanly(self):
        self.assertIn("wrote", migrate.migrate(self.spaces))
        self.commit("migrate")
        self.git("checkout", "-q", "-b", "lane-a")
        self.lane_edit_split(self.fa, self.n)
        self.commit("lane a")
        self.git("checkout", "-q", "-b", "lane-b", "main")
        self.lane_edit_split(self.fb, self.n + 1, status="blocked")
        self.commit("lane b")
        self.git("checkout", "-q", "main")
        for ref in ("lane-a", "lane-b"):
            r = self.merge(ref)
            self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
            self.assertEqual(self.unmerged(), [])
        got = self.combined()
        self.assertEqual(self.feature(got, self.fa)["status"], "in_progress")
        self.assertEqual(self.feature(got, self.fb)["status"], "blocked")
        self.assertIn(f"a1: evidence from {self.fb}'s lane", self.feature(got, self.fb)["checkpoints"][0]["evidence"])
        ids = [r["id"] for r in got["decisions"]["reconvene"]]
        self.assertEqual(ids[-2:], [f"RC-{self.n}", f"RC-{self.n + 1}"])
        self.assertEqual(planfiles.check(*planfiles.load_dir(self.spaces)[:3]), [])

    def test_single_file_layout_conflicts(self):
        """The problem SP-080 removes: the same two edits in the shared plan file collide."""
        self.git("checkout", "-q", "-b", "lane-a")
        self.lane_edit_combined(self.fa, self.n)
        self.commit("lane a")
        self.git("checkout", "-q", "-b", "lane-b", "main")
        self.lane_edit_combined(self.fb, self.n + 1)
        self.commit("lane b")
        self.git("checkout", "-q", "main")
        self.assertEqual(self.merge("lane-a").returncode, 0)
        self.assertNotEqual(self.merge("lane-b").returncode, 0)
        self.assertEqual(self.unmerged(), [f"{REL}/{planfiles.PLAN_NAME}"])


class ConvertBranchTest(Repo):
    """a4: a branch cut before the migration merges cleanly after converting."""

    def cut_lane_then_migrate_main(self, lane_status="in_progress"):
        self.git("checkout", "-q", "-b", "lane")
        self.lane_edit_combined(self.fa, self.n + 2, status=lane_status)
        self.commit("lane edits the plan file")
        self.git("checkout", "-q", "main")
        migrate.migrate(self.spaces)
        self.commit("migrate")
        plan = planfiles.load_dir(self.spaces)[0]
        plan["session_procedure"] = list(plan.get("session_procedure", [])) + ["new step"]
        self.write_plan(plan)
        self.commit("procedure edit on main")
        self.git("checkout", "-q", "-b", "other", "main")
        self.lane_edit_split(self.fc, self.n + 1, status="done")
        self.commit("another lane, already split")
        self.git("checkout", "-q", "main")
        self.assertEqual(self.merge("other").returncode, 0)
        self.git("checkout", "-q", "lane")

    def test_converts_and_merges_back_cleanly(self):
        self.cut_lane_then_migrate_main()
        self.assertNotEqual(self.merge("main").returncode, 0)
        self.assertIn(f"{REL}/{planfiles.PLAN_NAME}", self.unmerged())
        with contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(convert_branch.main(["--repo", self.repo]), 0)
        self.assertIn("wrote and staged", out.getvalue())
        self.assertEqual(self.unmerged(), [])
        self.git("commit", "-q", "--no-edit")
        plan, states, rcs, _ = planfiles.load_dir(self.spaces)
        self.assertEqual(planfiles.check(plan, states, rcs), [])
        got = planfiles.combine(*planfiles.load_dir(self.spaces))
        fa = self.feature(got, self.fa)
        self.assertEqual((fa["status"], fa["branch"]), ("in_progress", f"feat/{self.fa.lower()}-x"))
        self.assertIn(f"a1: evidence from {self.fa}'s lane", fa["checkpoints"][0]["evidence"])
        self.assertEqual(self.feature(got, self.fc)["status"], "done")
        self.assertEqual([r["id"] for r in got["decisions"]["reconvene"]][-2:], [f"RC-{self.n + 1}", f"RC-{self.n + 2}"])
        self.assertEqual(got["changelog"][-1]["change"], f"{self.fa} lane note")
        self.assertEqual(got["session_procedure"][-1], "new step")
        # Back on main the converted branch merges without a conflict.
        self.git("checkout", "-q", "main")
        r = self.merge("lane")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(planfiles.combine(*planfiles.load_dir(self.spaces)), got)

    def test_conflicting_edits_write_nothing(self):
        self.cut_lane_then_migrate_main()
        # main also changes the lane's feature, differently.
        self.git("checkout", "-q", "main")
        self.lane_edit_split(self.fa, self.n + 3, status="blocked")
        self.commit("main changes the same feature")
        self.git("checkout", "-q", "lane")
        self.merge("main")
        before = self.git("status", "--porcelain").stdout
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(convert_branch.main(["--repo", self.repo]), 1)
        self.assertIn(f"$.features[{self.fa}].status", err.getvalue())
        self.assertEqual(self.git("status", "--porcelain").stdout, before)

    def test_dry_run_and_explicit_refs(self):
        self.cut_lane_then_migrate_main()
        with contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(convert_branch.main(["--repo", self.repo, "--theirs", "main", "--dry-run"]), 0)
        self.assertIn("would write", out.getvalue())
        self.assertEqual(self.git("status", "--porcelain").stdout, "")

    def test_no_merge_in_progress(self):
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(convert_branch.main(["--repo", self.repo]), 2)
        self.assertIn("no merge in progress", err.getvalue())
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(convert_branch.main(["--repo", self.repo, "--theirs", "no-such-ref"]), 1)

    def test_refs_that_look_like_options_are_refused(self):
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(convert_branch.main(["--repo", self.repo, "--theirs=--output=x"]), 1)
            self.assertEqual(convert_branch.main(["--repo", self.repo, "--theirs", "main", "--base=-x"]), 1)
        self.assertIn("not a commit", err.getvalue())

    def test_read_ref_checks_file_names(self):
        migrate.migrate(self.spaces)
        os.rename(os.path.join(self.spaces, "state", f"{self.fa}.json"), os.path.join(self.spaces, "state", "SP-999.json"))
        self.commit("misnamed state file")
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(plantotals.main(["--repo", self.repo, "--ref", "HEAD"]), 1)
        self.assertIn("does not match the file name", err.getvalue())

    def test_migration_branch_catches_up_with_unmigrated_main(self):
        """The SP-080 branch itself: main kept editing the single file after the branch migrated."""
        self.git("checkout", "-q", "-b", "migration")
        migrate.migrate(self.spaces)
        self.commit("migrate")
        self.git("checkout", "-q", "main")
        self.lane_edit_combined(self.fb, self.n + 4, status="done")
        self.commit("main: a lane merged before the migration")
        expected = self.combined()
        self.git("checkout", "-q", "migration")
        self.merge("main")
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(convert_branch.main(["--repo", self.repo]), 0)
        self.git("commit", "-q", "--no-edit")
        plan, states, rcs, order = planfiles.load_dir(self.spaces)
        self.assertFalse(planfiles.is_combined(plan))
        self.assertEqual(order, [r["id"] for r in expected["decisions"]["reconvene"]])
        self.assertEqual(planfiles.dumps(planfiles.combine(plan, states, rcs, order)), planfiles.dumps(expected))

    def test_neither_side_migrated_merges_by_meaning(self):
        self.git("checkout", "-q", "-b", "lane")
        self.lane_edit_combined(self.fa, self.n + 2)
        self.commit("lane")
        self.git("checkout", "-q", "main")
        self.lane_edit_combined(self.fb, self.n + 1)
        self.commit("main")
        self.git("checkout", "-q", "lane")
        self.assertNotEqual(self.merge("main").returncode, 0)
        with contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(convert_branch.main(["--repo", self.repo]), 0)
        self.assertIn("single-file", out.getvalue())
        got = self.combined()
        self.assertTrue(planfiles.is_combined(planfiles.load_dir(self.spaces)[0]))
        self.assertTrue({r["id"] for r in got["decisions"]["reconvene"]} >= {f"RC-{self.n + 2}", f"RC-{self.n + 1}"})


class PlanMergeTest(unittest.TestCase):
    def test_rules(self):
        base = {"a": 1, "l": [1], "items": [{"id": "x", "v": 1}], "gone": 1}
        ours = {"a": 2, "l": [1, 2], "items": [{"id": "x", "v": 1}, {"id": "y", "v": 1}]}
        theirs = {"a": 1, "l": [1, 3], "items": [{"id": "x", "v": 5}], "gone": 1, "new": 1}
        merged, conflicts = planmerge.merge(base, ours, theirs)
        self.assertEqual(conflicts, [])
        self.assertEqual(merged, {"a": 2, "l": [1, 3, 2], "items": [{"id": "x", "v": 5}, {"id": "y", "v": 1}], "new": 1})
        _, conflicts = planmerge.merge({"a": 1, "b": 1}, {"a": 2}, {"a": 3, "b": 2})
        self.assertEqual(conflicts, ["$.a", "$.b: deleted on one side, changed on the other"])

    def test_cli(self):
        d = tempfile.mkdtemp(prefix="sp-080-pm-")
        self.addCleanup(shutil.rmtree, d)
        paths = [os.path.join(d, n) for n in ("b", "o", "t", "out")]
        for p, v in zip(paths, ('{"a": 1}', '{"a": 2}', '{"a": 1, "b": 1}')):
            with open(p, "w") as fh:
                fh.write(v)
        self.assertEqual(planmerge.main(paths), 0)
        with open(paths[3]) as fh:
            self.assertEqual(fh.read(), '{\n  "a": 2,\n  "b": 1\n}\n')
        with open(paths[2], "w") as fh:
            fh.write('{"a": 3}')
        with contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(planmerge.main(paths), 1)
        self.assertIn("$.a", out.getvalue())
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(planmerge.main([]), 2)


if __name__ == "__main__":
    unittest.main()
