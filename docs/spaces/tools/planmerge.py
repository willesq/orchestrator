"""Three-way merge of plan JSON by meaning (SP-080).

Usage: python3 planmerge.py BASE OURS THEIRS OUT

Lists of objects with an 'id' merge by id; other lists merge as appends when both
sides only appended; scalars take the side that changed. Exits 1 listing every path
both sides changed differently, and writes nothing then. convert_branch.py uses the
same merge on the combined view of three commits.

Standard library only.
"""

from __future__ import annotations

import json
import sys

MISSING = object()


def merge(base, ours, theirs):
    """Return (merged, conflicts); conflicts lists JSON paths both sides changed differently."""
    conflicts = []
    result = _merge(base, ours, theirs, "$", conflicts)
    return result, conflicts


def _merge(b, o, t, path, conflicts):
    if o == t:
        return o
    if o == b:
        return t
    if t == b:
        return o
    if isinstance(o, dict) and isinstance(t, dict):
        bd = b if isinstance(b, dict) else {}
        out = {}
        for k in list(t.keys()) + [k for k in o if k not in t]:
            r = _merge(bd.get(k, MISSING), o.get(k, MISSING), t.get(k, MISSING), f"{path}.{k}", conflicts)
            if r is not MISSING:
                out[k] = r
        return out
    if isinstance(o, list) and isinstance(t, list):
        bl = b if isinstance(b, list) else []
        if all(isinstance(x, dict) and "id" in x for x in o + t):
            bi = {x["id"]: x for x in bl if isinstance(x, dict) and "id" in x}
            oi = {x["id"]: x for x in o}
            ti = {x["id"]: x for x in t}
            out = []
            for x in t:
                r = _merge(bi.get(x["id"], MISSING), oi.get(x["id"], MISSING), x, f"{path}[{x['id']}]", conflicts)
                if r is not MISSING:
                    out.append(r)
            for x in o:
                if x["id"] not in ti and x["id"] not in bi:
                    out.append(x)
            return out
        if o[: len(bl)] == bl and t[: len(bl)] == bl:
            return t + [x for x in o[len(bl):] if x not in t]
    if o is MISSING or t is MISSING:
        conflicts.append(f"{path}: deleted on one side, changed on the other")
        return o if t is MISSING else t
    conflicts.append(path)
    return o


def main(argv=None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    if len(argv) != 4:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2
    docs = []
    for p in argv[:3]:
        with open(p, encoding="utf-8") as fh:
            docs.append(json.load(fh))
    result, conflicts = merge(*docs)
    if conflicts:
        print("CONFLICTS:\n" + "\n".join(conflicts))
        return 1
    with open(argv[3], "w", encoding="utf-8") as fh:
        fh.write(json.dumps(result, indent=2, ensure_ascii=False) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
