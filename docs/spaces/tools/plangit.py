"""Read the spaces plan files from a git commit without checking it out (SP-080).

Standard library only.
"""

from __future__ import annotations

import json
import subprocess

import planfiles

SPACES = "docs/spaces"


def git(repo: str, *args: str, data: bytes = None) -> bytes:
    """Run git in repo and return stdout; raise CalledProcessError with stderr on failure."""
    return subprocess.run(
        ["git", "-C", repo, *args], input=data, check=True, capture_output=True
    ).stdout


def _blobs(repo: str, specs):
    """Contents of each 'ref:path' in specs, through one git cat-file process."""
    if not specs:
        return []
    out = git(repo, "cat-file", "--batch", data=("\n".join(specs) + "\n").encode())
    blobs, pos = [], 0
    for spec in specs:
        nl = out.index(b"\n", pos)
        header = out[pos:nl].split()
        if len(header) != 3:
            raise planfiles.PlanError(f"git cannot read {spec}")
        size = int(header[2])
        blobs.append(out[nl + 1 : nl + 1 + size])
        pos = nl + 1 + size + 1
    return blobs


def resolve(repo: str, ref: str) -> str:
    """The commit SHA ref names. Refs come from the command line, so one that looks like
    an option is refused and only the SHA is passed on to later git commands."""
    if not ref or ref.startswith("-"):
        raise planfiles.PlanError(f"not a commit: {ref!r}")
    try:
        out = git(repo, "rev-parse", "--verify", "-q", "--end-of-options", ref + "^{commit}")
    except subprocess.CalledProcessError:
        raise planfiles.PlanError(f"not a commit: {ref!r}") from None
    return out.decode().strip()


def read_ref(repo: str, ref: str, spaces: str = SPACES):
    """(plan, states, rcs, order) as committed at ref."""
    sha = resolve(repo, ref)
    plan = json.loads(git(repo, "show", f"{sha}:{spaces}/{planfiles.PLAN_NAME}"))
    names = git(
        repo, "ls-tree", "-r", "-z", "--name-only", sha, "--",
        f"{spaces}/{planfiles.STATE_DIR}", f"{spaces}/{planfiles.RC_DIR}",
    ).decode().split("\0")
    names = [n for n in names if n.endswith(".json")]
    states, rcs, order = {}, {}, None
    for name, blob in zip(names, _blobs(repo, [f"{sha}:{n}" for n in names])):
        obj = json.loads(blob)
        base = name.rsplit("/", 1)[-1]
        if base == planfiles.ORDER_NAME and name.startswith(f"{spaces}/{planfiles.RC_DIR}/"):
            order = obj
            continue
        is_state = name.startswith(f"{spaces}/{planfiles.STATE_DIR}/")
        pattern = planfiles.FEATURE_ID if is_state else planfiles.RC_ID
        oid = obj.get("id") if isinstance(obj, dict) else None
        if not isinstance(oid, str) or not pattern.match(oid) or base != oid + ".json":
            raise planfiles.PlanError(f"{name} at {ref}: id {oid!r} does not match the file name")
        (states if is_state else rcs)[oid] = obj
    return plan, states, rcs, order
