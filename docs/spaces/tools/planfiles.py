"""Read, combine and split the spaces plan and its per-feature files (SP-080, D-80).

Layout under docs/spaces/:

  spaces-features.json        definitions, criteria, decisions, changelog, procedure
  state/<ID>.json             one feature's mutable fields and checkpoint evidence
  reconvene/<RC-id>.json      one reconvene item
  reconvene/order.json        the order reconvene items had when they were migrated

The combined view is the single-file plan as it looked before SP-080: every feature
carries its state fields, every checkpoint its evidence, and decisions.reconvene lists
every item. Readers combine; writers split. A plan that still embeds its state (a branch
cut before the migration) combines to itself, so tools work on either layout.

Standard library only.
"""

from __future__ import annotations

import copy
import json
import os
import re

PLAN_NAME = "spaces-features.json"
STATE_DIR = "state"
RC_DIR = "reconvene"
ORDER_NAME = "order.json"

# The feature fields a lane changes while it works; everything else is a definition.
STATE_FIELDS = (
    "status",
    "passes",
    "verified_sha",
    "verified_at",
    "blocked_reason",
    "attempts",
    "branch",
    "pr",
)
DEFAULT_STATE = {
    "status": "todo",
    "passes": False,
    "verified_sha": None,
    "verified_at": None,
    "blocked_reason": None,
    "attempts": 0,
    "branch": None,
    "pr": None,
}
# Key order of a feature in the combined view, so a combined plan serialises
# byte for byte like the pre-migration file. Unknown keys keep their place after these.
FEATURE_KEY_ORDER = (
    "id",
    "phase",
    "priority",
    "title",
    "summary",
    "why",
    "depends_on",
    "touches",
    "docs_to_update",
    "checkpoints",
    "status",
    "passes",
    "verified_sha",
    "verified_at",
    "blocked_reason",
    "attempts",
    "criteria_sha256",
    "branch",
    "pr",
)
CHECKPOINT_KEY_ORDER = ("id", "title", "acceptance", "evidence")

FEATURE_ID = re.compile(r"SP-\d+\Z")
RC_ID = re.compile(r"RC-(\d+)\Z")


class PlanError(ValueError):
    """The files cannot be combined without guessing."""


def dumps(obj) -> str:
    """Serialise like the plan file: two-space indent, UTF-8 text, final newline."""
    return json.dumps(obj, indent=2, ensure_ascii=False) + "\n"


def rc_sort_key(rc_id: str):
    m = RC_ID.match(rc_id)
    return (int(m.group(1)) if m else float("inf"), rc_id)


def _ordered(d: dict, order) -> dict:
    out = {k: d[k] for k in order if k in d}
    out.update((k, v) for k, v in d.items() if k not in out)
    return out


def is_combined(plan: dict) -> bool:
    """True when the plan file still embeds state or reconvene items (pre-SP-080 layout)."""
    if "reconvene" in plan.get("decisions", {}):
        return True
    for f in plan.get("features", []):
        if any(k in f for k in STATE_FIELDS):
            return True
        if any("evidence" in c for c in f.get("checkpoints", [])):
            return True
    return False


def combine(plan: dict, states: dict, rcs: dict, order=None) -> dict:
    """Build the combined view.

    states maps feature id to its state file, rcs maps RC id to its item, order lists
    RC ids in their migrated order. Items not in order follow by RC number, so a new
    item never needs order.json edited. Raises PlanError when the plan embeds a value a
    file also holds, because then one of them would be silently dropped.
    """
    d = copy.deepcopy(plan)
    known = set()
    for i, f in enumerate(d.get("features", [])):
        fid = f["id"]
        known.add(fid)
        s = states.get(fid)
        if s is not None:
            clash = [k for k in STATE_FIELDS if k in f and k in s]
            if clash:
                raise PlanError(f"{fid}: {', '.join(clash)} set in both the plan and state/{fid}.json")
        s = s or {}
        for k in STATE_FIELDS:
            if k in s:
                f[k] = s[k]
            elif k not in f:
                f[k] = DEFAULT_STATE[k]
        ev = s.get("evidence", {})
        cids = set()
        for j, c in enumerate(f.get("checkpoints", [])):
            cids.add(c["id"])
            if c["id"] in ev:
                if "evidence" in c and c["evidence"]:
                    raise PlanError(f"{c['id']}: evidence in both the plan and state/{fid}.json")
                c["evidence"] = copy.deepcopy(ev[c["id"]])
            elif "evidence" not in c:
                c["evidence"] = []
            f["checkpoints"][j] = _ordered(c, CHECKPOINT_KEY_ORDER)
        stray = sorted(set(ev) - cids)
        if stray:
            raise PlanError(f"state/{fid}.json: evidence for unknown checkpoints {', '.join(stray)}")
        d["features"][i] = _ordered(f, FEATURE_KEY_ORDER)
    orphans = sorted(set(states) - known)
    if orphans:
        raise PlanError(f"state files for features not in the plan: {', '.join(orphans)}")

    decisions = d.setdefault("decisions", {})
    items = list(decisions.get("reconvene", []))
    seen = {r["id"] for r in items}
    clash = sorted(seen & set(rcs))
    if clash:
        raise PlanError(f"reconvene items in both the plan and reconvene/: {', '.join(clash)}")
    pending = {k: copy.deepcopy(v) for k, v in rcs.items()}
    for rc_id in order or []:
        if rc_id in pending:
            items.append(pending.pop(rc_id))
    items.extend(pending[k] for k in sorted(pending, key=rc_sort_key))
    decisions.pop("reconvene", None)
    decisions["reconvene"] = items
    return d


def split(combined: dict):
    """Inverse of combine: return (plan, states, rcs, order) for a combined view."""
    plan = copy.deepcopy(combined)
    states = {}
    for f in plan.get("features", []):
        s = {"id": f["id"]}
        for k in STATE_FIELDS:
            s[k] = f.pop(k, DEFAULT_STATE[k])
        s["evidence"] = {c["id"]: c.pop("evidence", []) for c in f.get("checkpoints", [])}
        states[f["id"]] = s
    items = plan.get("decisions", {}).pop("reconvene", [])
    rcs = {r["id"]: r for r in items}
    if len(rcs) != len(items):
        raise PlanError("duplicate reconvene ids")
    return plan, states, rcs, [r["id"] for r in items]


def default_state(feature: dict) -> dict:
    s = {"id": feature["id"], **copy.deepcopy(DEFAULT_STATE)}
    s["evidence"] = {c["id"]: [] for c in feature.get("checkpoints", [])}
    return s


def _read_json(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def load_dir(spaces_dir: str):
    """Read (plan, states, rcs, order) from a docs/spaces directory."""
    plan = _read_json(os.path.join(spaces_dir, PLAN_NAME))
    states, rcs, order = {}, {}, None
    sdir = os.path.join(spaces_dir, STATE_DIR)
    if os.path.isdir(sdir):
        for name in sorted(os.listdir(sdir)):
            if not name.endswith(".json"):
                continue
            s = _read_json(os.path.join(sdir, name))
            if f"{s.get('id')}.json" != name or not FEATURE_ID.match(str(s.get("id"))):
                raise PlanError(f"{STATE_DIR}/{name}: id {s.get('id')!r} does not match the file name")
            states[s["id"]] = s
    rdir = os.path.join(spaces_dir, RC_DIR)
    if os.path.isdir(rdir):
        for name in sorted(os.listdir(rdir)):
            if name == ORDER_NAME:
                order = _read_json(os.path.join(rdir, name))
                continue
            if not name.endswith(".json"):
                continue
            r = _read_json(os.path.join(rdir, name))
            if f"{r.get('id')}.json" != name or not RC_ID.match(str(r.get("id"))):
                raise PlanError(f"{RC_DIR}/{name}: id {r.get('id')!r} does not match the file name")
            rcs[r["id"]] = r
    return plan, states, rcs, order


def load_combined(spaces_dir: str) -> dict:
    return combine(*load_dir(spaces_dir))


def _write(path, obj):
    """Write obj as plan JSON, replacing the file rather than writing through it.

    A merged branch could commit a symlink in place of a state file or folder; writing
    through it would overwrite whatever it points at, so links are refused.
    """
    parent = os.path.dirname(path)
    os.makedirs(parent, exist_ok=True)
    if os.path.islink(path) or os.path.islink(parent):
        raise PlanError(f"refusing to write through a symlink: {path}")
    text = dumps(obj)
    if os.path.exists(path):
        with open(path, encoding="utf-8") as fh:
            if fh.read() == text:
                return
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(text)
    os.replace(tmp, path)


def write_split(spaces_dir: str, plan, states, rcs, order=None, prune=True) -> None:
    """Write the split layout. With prune, state and reconvene files not in the
    result are removed, so the directory holds exactly what was given."""
    # File names come from ids in the JSON; refuse anything that is not a plain id so a
    # malformed id cannot write or delete outside state/ and reconvene/.
    def plain(k, pattern):
        return isinstance(k, str) and pattern.match(k)

    bad = [k for k in states if not plain(k, FEATURE_ID)] + [k for k in rcs if not plain(k, RC_ID)]
    if bad:
        raise PlanError(f"ids not usable as file names: {', '.join(map(repr, bad))}")
    _write(os.path.join(spaces_dir, PLAN_NAME), plan)
    for fid, s in states.items():
        _write(os.path.join(spaces_dir, STATE_DIR, fid + ".json"), s)
    for rc_id, r in rcs.items():
        _write(os.path.join(spaces_dir, RC_DIR, rc_id + ".json"), r)
    if order is not None:
        _write(os.path.join(spaces_dir, RC_DIR, ORDER_NAME), order)
    if not prune:
        return
    for sub, keep in ((STATE_DIR, states), (RC_DIR, rcs)):
        d = os.path.join(spaces_dir, sub)
        if not os.path.isdir(d):
            continue
        for name in os.listdir(d):
            stem = name[:-5] if name.endswith(".json") else None
            if stem and stem not in keep and (FEATURE_ID.match(stem) or RC_ID.match(stem)):
                os.remove(os.path.join(d, name))


def check(plan, states, rcs) -> list:
    """Problems with a split layout; empty when the files are consistent."""
    problems = []
    if is_combined(plan):
        problems.append(f"{PLAN_NAME} still embeds state fields, evidence or reconvene items")
    valid = set(plan.get("status_values", {})) or set()
    for fid, s in states.items():
        if valid and s.get("status") not in valid:
            problems.append(f"{STATE_DIR}/{fid}.json: unknown status {s.get('status')!r}")
        extra = set(s) - set(STATE_FIELDS) - {"id", "evidence"}
        if extra:
            problems.append(f"{STATE_DIR}/{fid}.json: unexpected fields {', '.join(sorted(extra))}")
    for rc_id, r in rcs.items():
        for k in ("kind", "topic", "detail", "source", "raised", "affects", "status"):
            if k not in r:
                problems.append(f"{RC_DIR}/{rc_id}.json: missing {k}")
    try:
        combine(plan, states, rcs)
    except PlanError as e:
        problems.append(str(e))
    return problems
