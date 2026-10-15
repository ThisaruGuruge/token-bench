"""Compares the GA and GB outputs of the parity pairs.
usage: python3 compare-parity.py <pairs.json> <work dir> <seed>"""
import json, os, sys

pairs = {pair["id"]: pair for pair in json.load(open(sys.argv[1]))}
work, seed = sys.argv[2], sys.argv[3]
runs = {grammar: {run["id"]: run for run in json.load(open(f"{work}/out/{grammar}.json"))} for grammar in "AB"}
COMMAND_KEYS = {"command", "commands", "next", "suggestion"}


def strip(value):
    """Drops printed commands and the grammar-worded suggestion, recursively."""
    if isinstance(value, dict):
        return {key: strip(item) for key, item in value.items() if key not in COMMAND_KEYS}
    if isinstance(value, list):
        return [strip(item) for item in value]
    if isinstance(value, str) and value.startswith("bal discover "):
        return "<command>"
    return value


def parse(text):
    try:
        return json.loads(text) if text.strip() else None
    except json.JSONDecodeError:
        return "<not json>"


failures, notes = [], []
for pair_id, pair in pairs.items():
    a, b = runs["A"][pair_id], runs["B"][pair_id]
    problems = []
    if a["exit"] != b["exit"]:
        problems.append(f"exit A={a['exit']} B={b['exit']}")
    elif pair["expectExit"] is not None and a["exit"] != pair["expectExit"]:
        problems.append(f"exit {a['exit']}, expected {pair['expectExit']}")
    if a["exit"] == 0 and b["exit"] == 0:
        left, right = strip(parse(a["stdout"])), strip(parse(b["stdout"]))
        if left != right:
            problems.append("answer content differs after removing commands")
    else:
        left, right = parse(a["stderr"]), parse(b["stderr"])
        kind_a = left.get("kind") if isinstance(left, dict) else left
        kind_b = right.get("kind") if isinstance(right, dict) else right
        if kind_a != kind_b:
            problems.append(f"failure kind A={kind_a} B={kind_b}")
        elif isinstance(left, dict) and isinstance(right, dict) and left.get("message") != right.get("message"):
            notes.append(f"{pair_id}: messages differ: A={left.get('message')!r} B={right.get('message')!r}")
        if a["stdout"].strip() or b["stdout"].strip():
            problems.append("a failure printed to stdout")
    status = "ok  " if not problems else "FAIL"
    print(f"  {status}  {pair_id:24} exit A={a['exit']} B={b['exit']}  {'; '.join(problems)}")
    if problems:
        failures.append(pair_id)

# Live fetches: files each run's cache gained beyond the seed (expected: the
# unknown-package/version lookups only, which go to Central in both arms).
for grammar in "AB":
    cache, added = f"{work}/cache-G{grammar}", []
    for directory, _, files in os.walk(cache):
        for name in files:
            relative = os.path.relpath(os.path.join(directory, name), cache)
            if not os.path.exists(os.path.join(seed, relative)):
                added.append(relative)
    notes.append(f"G{grammar} cache files beyond the seed: {len(added)} {sorted(added)[:6]}")

print("\n".join("  note  " + note for note in notes))
print(f"PARITY {'PASS' if not failures else 'FAIL'}: {len(pairs) - len(failures)}/{len(pairs)} pairs agree")
sys.exit(1 if failures else 0)
