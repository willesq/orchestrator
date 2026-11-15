"""The reader (docs/spaces/index.html) shows the same totals before and after the
SP-080 migration, and the same totals as plantotals.py (SP-080.1 a3).

It serves a copy of docs/spaces over http://localhost, the way the reader documents,
renders it in headless Chrome and reads the numbers from the page. Skipped when no
Chrome or Chromium is installed; set SPACES_TEST_CHROME to a browser binary to choose one.

Run: python3 -m unittest discover -s docs/spaces/tools
"""

from __future__ import annotations

import functools
import http.server
import os
import re
import shutil
import subprocess
import tempfile
import threading
import unittest
from collections import Counter

import migrate
import planfiles
import plantotals

SPACES = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CANDIDATES = (
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/Applications/Chromium.app/Contents/MacOS/Chromium",
    "google-chrome", "chromium", "chromium-browser",
)


def find_chrome():
    for c in (os.environ.get("SPACES_TEST_CHROME"),) + CANDIDATES:
        if c and (os.path.isfile(c) or shutil.which(c)):
            return c if os.path.isfile(c) else shutil.which(c)
    return None


class _Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


def render(chrome: str, directory: str, profile: str) -> str:
    """The reader's DOM after it has loaded the files in directory."""
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(_Quiet, directory=directory))
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    try:
        url = f"http://localhost:{server.server_address[1]}/index.html"
        out = os.path.join(profile + ".dom.html")
        # The DOM goes to a file, not a pipe: Chrome's helper processes inherit and hold
        # a pipe open after the browser exits, so reading one would wait on them.
        with open(out, "wb") as fh:
            subprocess.run(
                [chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
                 f"--user-data-dir={profile}", "--virtual-time-budget=20000", "--dump-dom", url],
                stdout=fh, stderr=subprocess.DEVNULL, timeout=120, check=False,
            )
        with open(out, encoding="utf-8") as fh:
            return fh.read()
    finally:
        server.shutdown()
        server.server_close()


def page_totals(dom: str) -> dict:
    """The numbers the reader shows, keyed like plantotals.totals."""
    stats = dict((label, value) for value, label in re.findall(r'<div class="stat"><b>([^<]*)</b><span>([^<]*)</span>', dom))
    if not stats:
        raise AssertionError("the reader rendered no totals:\n" + dom[:2000])
    status = Counter(re.findall(r'<details class="feat[^"]*" id="SP-\d+" data-status="(\w+)"', dom))
    rc_rows = re.findall(r'<td class="mono">(RC-\d+)</td>', dom)
    done, feats = stats["features done"].split(" / ")
    cps_done, cps = stats["checkpoints with evidence"].split(" / ")
    acc_done, acc = stats["acceptance checks covered"].split(" / ")
    return {
        "features": int(feats),
        "done": int(done),
        "status": dict(status),
        "checkpoints": int(cps),
        "checkpoints_with_evidence": int(cps_done),
        "acceptance": int(acc),
        "acceptance_covered": int(acc_done),
        "in_progress": int(stats["in progress"]),
        "blocked": int(stats["blocked"]),
        "open_decisions": int(stats["open decisions"]),
        "reconvene_total": len(rc_rows),
        "reconvene_ids": rc_rows,
        "reconvene_open": int(stats["to reconvene on"]),
        "hash_note": (re.search(r'id="hashNote">([^<]*)<', dom) or [None, ""])[1],
    }


def expected(t: dict, combined: dict) -> dict:
    return {
        "features": t["features"],
        "done": t["status"].get("done", 0),
        "status": {k: v for k, v in t["status"].items() if v},
        "checkpoints": t["checkpoints"],
        "checkpoints_with_evidence": t["checkpoints_with_evidence"],
        "acceptance": t["acceptance"],
        "acceptance_covered": t["acceptance_covered"],
        "in_progress": t["status"].get("in_progress", 0),
        "blocked": t["status"].get("blocked", 0),
        "open_decisions": t["open_decisions"],
        "reconvene_total": t["reconvene"]["total"],
        "reconvene_ids": [r["id"] for r in combined["decisions"]["reconvene"]],
        "reconvene_open": t["reconvene"].get("open", 0),
    }


@unittest.skipUnless(find_chrome(), "no Chrome or Chromium for the headless reader check")
class ReaderTotalsTest(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="sp-080-reader-")
        self.addCleanup(shutil.rmtree, self.root)
        self.chrome = find_chrome()

    def site(self, name, combined, migrated):
        d = os.path.join(self.root, name)
        os.makedirs(d)
        shutil.copy(os.path.join(SPACES, "index.html"), d)
        with open(os.path.join(d, planfiles.PLAN_NAME), "w", encoding="utf-8") as fh:
            fh.write(planfiles.dumps(combined))
        if migrated:
            migrate.migrate(d)
        return d

    def test_same_totals_before_and_after_migration(self):
        combined = planfiles.load_combined(SPACES)
        want = expected(plantotals.totals(combined), combined)
        got = {}
        for name, migrated in (("before", False), ("after", True)):
            d = self.site(name, combined, migrated)
            self.assertEqual(plantotals.totals(planfiles.load_combined(d)), plantotals.totals(combined))
            got[name] = page_totals(render(self.chrome, d, os.path.join(self.root, "profile-" + name)))
            note = got[name].pop("hash_note")
            self.assertRegex(note, r"^All \d+ criteria hashes match", name)
        self.assertEqual(got["before"], want)
        self.assertEqual(got["after"], want)


if __name__ == "__main__":
    unittest.main()
