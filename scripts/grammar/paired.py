#!/usr/bin/env python3
"""Paired grammar comparison (design §5-§6): GB vs GA per metric, blocked by
task, with a two-stage bootstrap CI and a Wilcoxon signed-rank check, judged
against the pre-registered thresholds.

usage: paired.py <results dir> [--resamples N] [--seed N] [--json out.json] [--csv per-task.csv]

Reads every result.json below the directory (dot-directories, such as
.infra-failures, are skipped). Infrastructure failures are excluded. Runs with a
tool exit 4 or 5 are kept and also re-analysed without them (sensitivity)."""
import argparse
import csv
import json
import math
import os
import random
import statistics
import sys

ARMS = ("GA", "GB")
# metric: (label, kind, threshold). kind "diff" = B-A on the raw scale (pp for
# proportions), "ratio" = exp(mean log(B/A)), skewed positive metrics.
METRICS = {
    "success": ("Success (all fields)", "diff", 0.10),
    "partial": ("Partial score", "diff", None),
    "cost": ("Cost USD (Claude Code)", "ratio", 0.15),
    "tokens": ("Total tokens", "ratio", 0.15),
    "commands": ("Discover commands", "ratio", 0.15),
    "failed": ("Failed commands", "diff", 0.5),
    "help": ("Help calls", "diff", None),
    "bytes": ("Bytes read from the tool", "ratio", None),
    "toolTokens": ("Tool output tokens (est.)", "ratio", None),
    "firstValid": ("First command valid", "diff", None),
    "firstOnTarget": ("First command on target", "diff", 0.15),
    "agentSeconds": ("Agent time s (wall - tool)", "ratio", 0.20),
    "wallSeconds": ("Wall time s", "ratio", None),
    "recovered": ("Recovered error episodes", "diff", None),
}


def load(root):
    runs = []
    for directory, dirs, files in os.walk(root):
        dirs[:] = [d for d in dirs if not d.startswith(".")]
        if "result.json" not in files:
            continue
        value = json.load(open(os.path.join(directory, "result.json")))
        if value.get("arm") not in ARMS or value.get("infraFailure"):
            continue
        runs.append(value)
    return runs


def metrics_of(run):
    discover = run.get("discover") or {}
    answer = run.get("answer") or {}
    tokens = run.get("tokens", {}).get("total", 0)
    wall = run.get("durationSeconds", 0.0)
    tool = discover.get("toolMs", 0) / 1000
    on_target = discover.get("firstOnTarget")
    return {
        "success": 1.0 if run.get("passed") else 0.0,
        "partial": answer.get("correct", 0) / answer["total"] if answer.get("total") else 0.0,
        "cost": run.get("reportedCostUsd") if run.get("reportedCostUsd") is not None else run.get("costUsd"),
        "tokens": tokens,
        "commands": discover.get("calls", 0),
        "failed": discover.get("failed", 0),
        "help": discover.get("helpCalls", 0),
        "bytes": discover.get("totalBytes", 0),
        "toolTokens": discover.get("estimatedTokens", 0),
        "firstValid": 1.0 if discover.get("firstValid") else 0.0,
        "firstOnTarget": None if on_target is None else (1.0 if on_target else 0.0),
        "agentSeconds": max(wall - tool, 0.0),
        "wallSeconds": wall,
        "recovered": discover.get("recoveredEpisodes", 0),
    }


def cells(runs):
    table = {}
    for run in runs:
        table.setdefault(run["task"], {}).setdefault(run["arm"], []).append(metrics_of(run))
    return {task: arms for task, arms in table.items() if all(arm in arms for arm in ARMS)}


def cell_mean(values):
    present = [value for value in values if value is not None]
    return statistics.fmean(present) if present else None


def effect(table, metric, kind):
    """Mean over tasks of B-A (diff) or of log(B/A) (ratio); None if undefined."""
    deltas = []
    for arms in table.values():
        a = cell_mean([m[metric] for m in arms["GA"]])
        b = cell_mean([m[metric] for m in arms["GB"]])
        if a is None or b is None:
            continue
        if kind == "ratio":
            if a <= 0 or b <= 0:
                a, b = a + 1, b + 1  # counts can be zero; shift by one for the log ratio
            deltas.append(math.log(b / a))
        else:
            deltas.append(b - a)
    return deltas


def bootstrap(table, metric, kind, resamples, rng):
    tasks = list(table)
    estimates = []
    for _ in range(resamples):
        sample = {}
        for index, task in enumerate(rng.choices(tasks, k=len(tasks))):
            sample[(task, index)] = {arm: rng.choices(table[task][arm], k=len(table[task][arm])) for arm in ARMS}
        deltas = effect(sample, metric, kind)
        if deltas:
            estimates.append(statistics.fmean(deltas))
    estimates.sort()
    if not estimates:
        return None, None
    return estimates[int(0.025 * len(estimates))], estimates[min(int(0.975 * len(estimates)), len(estimates) - 1)]


def wilcoxon(deltas):
    """Two-sided signed-rank p-value, normal approximation (zeros dropped)."""
    nonzero = [delta for delta in deltas if abs(delta) > 1e-12]
    n = len(nonzero)
    if n < 5:
        return None
    ordered = sorted(nonzero, key=abs)
    ranks, index = [0.0] * n, 0
    while index < n:
        end = index
        while end + 1 < n and abs(abs(ordered[end + 1]) - abs(ordered[index])) < 1e-12:
            end += 1
        for position in range(index, end + 1):
            ranks[position] = (index + end) / 2 + 1
        index = end + 1
    w = sum(rank for rank, delta in zip(ranks, ordered) if delta > 0)
    mean, sd = n * (n + 1) / 4, math.sqrt(n * (n + 1) * (2 * n + 1) / 24)
    z = (w - mean) / sd
    return math.erfc(abs(z) / math.sqrt(2))


def verdict(kind, point, low, high, threshold):
    if threshold is None or point is None or low is None:
        return ""
    if kind == "ratio":
        point, low, high = math.exp(point) - 1, math.exp(low) - 1, math.exp(high) - 1
    if low > threshold:
        return "B higher, material"
    if high < -threshold:
        return "B lower, material"
    if -threshold <= low and high <= threshold:
        return "no material difference"
    return "inconclusive"


def describe(kind, value):
    if value is None:
        return "-"
    return f"{math.exp(value):.2f}x" if kind == "ratio" else f"{value:+.3f}"


def arm_summary(runs):
    summary = {}
    for arm in ARMS:
        mine = [run for run in runs if run["arm"] == arm]
        values = [metrics_of(run) for run in mine]
        histogram = {}
        for run in mine:
            for code, count in ((run.get("discover") or {}).get("failedByExit") or {}).items():
                histogram[code] = histogram.get(code, 0) + count

        def mean_of(key):
            present = [value[key] for value in values if value[key] is not None]
            return statistics.fmean(present) if present else None

        summary[arm] = {
            "runs": len(mine),
            "passed": sum(1 for run in mine if run.get("passed")),
            "costTotal": sum(value["cost"] or 0 for value in values),
            **{key: mean_of(key) for key in METRICS},
            "failedByExit": dict(sorted(histogram.items())),
            "helpFirstRate": statistics.fmean([1.0 if (run.get("discover") or {}).get("helpFirst") else 0.0 for run in mine]) if mine else None,
            "bypassRuns": sum(1 for run in mine if (run.get("discover") or {}).get("bypasses")),
            "liveFetches": sum((run.get("discover") or {}).get("liveFetches", 0) for run in mine),
            "noSuccessfulCall": sum(1 for run in mine if not (run.get("discover") or {}).get("successful")),
            "stopReasons": sorted({run["stopReason"] for run in mine if run.get("stopReason")}),
        }
    return summary


def analyse(runs, resamples, seed):
    table = cells(runs)
    rng = random.Random(seed)
    rows = []
    for metric, (label, kind, threshold) in METRICS.items():
        deltas = effect(table, metric, kind)
        point = statistics.fmean(deltas) if deltas else None
        low, high = bootstrap(table, metric, kind, resamples, rng) if deltas else (None, None)
        rows.append({
            "metric": metric, "label": label, "kind": kind, "tasks": len(deltas),
            "effect": describe(kind, point), "ci": f"[{describe(kind, low)}, {describe(kind, high)}]",
            "wilcoxonP": wilcoxon(deltas), "threshold": threshold,
            "verdict": verdict(kind, point, low, high, threshold),
        })
    return table, rows


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("root")
    parser.add_argument("--resamples", type=int, default=10000)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--json")
    parser.add_argument("--csv")
    options = parser.parse_args()
    runs = load(options.root)
    if not runs:
        sys.exit("no GA/GB results found")
    table, rows = analyse(runs, options.resamples, options.seed)
    summary = arm_summary(runs)
    flagged = [run for run in runs if set(((run.get("discover") or {}).get("failedByExit") or {})) & {"4", "5"}]
    sensitivity = analyse([run for run in runs if run not in flagged], options.resamples, options.seed)[1] if flagged else None

    print(f"Runs: {len(runs)} ({', '.join(f'{arm} {summary[arm]['runs']}' for arm in ARMS)}); paired tasks: {len(table)}\n")
    print("| Arm | Success | Cost total | Cost/run | Commands | Failed | Exit codes | Help | Bytes | Tool tokens | Wall s | First valid | On target | Help first |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for arm in ARMS:
        s = summary[arm]
        fmt = lambda v, p=2: "-" if v is None else f"{v:.{p}f}"
        print(f"| {arm} | {s['passed']}/{s['runs']} | ${s['costTotal']:.3f} | ${fmt(s['cost'], 4)} | {fmt(s['commands'], 1)} | {fmt(s['failed'], 1)} | "
              f"{s['failedByExit'] or '-'} | {fmt(s['help'], 1)} | {fmt(s['bytes'], 0)} | {fmt(s['toolTokens'], 0)} | {fmt(s['wallSeconds'], 0)} | "
              f"{fmt(s['firstValid'])} | {fmt(s['firstOnTarget'])} | {fmt(s['helpFirstRate'])} |")
    print("\n| Metric | B vs A | 95% CI (two-stage bootstrap) | Wilcoxon p | Threshold | Verdict |")
    print("|---|---|---|---|---|---|")
    for row in rows:
        p = "-" if row["wilcoxonP"] is None else f"{row['wilcoxonP']:.3f}"
        threshold = "-" if row["threshold"] is None else (f"±{row['threshold']:.0%}" if row["kind"] == "ratio" else f"±{row['threshold']}")
        print(f"| {row['label']} | {row['effect']} | {row['ci']} | {p} | {threshold} | {row['verdict']} |")
    if flagged:
        print(f"\nSensitivity: {len(flagged)} runs with a tool exit 4/5 excluded")
        for row in sensitivity:
            print(f"  {row['label']}: {row['effect']} {row['ci']} {row['verdict']}")
    other = {arm: (summary[arm]["bypassRuns"], summary[arm]["liveFetches"], summary[arm]["noSuccessfulCall"], summary[arm]["stopReasons"]) for arm in ARMS}
    print("\nPer arm (bypass runs, live fetches, runs without a successful discover call, stop reasons):", other)

    if options.csv:
        with open(options.csv, "w", newline="") as handle:
            writer = csv.writer(handle)
            writer.writerow(["task", "metric", "GA mean", "GB mean", "GA runs", "GB runs"])
            for task in sorted(table):
                for metric in METRICS:
                    writer.writerow([task, metric, cell_mean([m[metric] for m in table[task]["GA"]]),
                                     cell_mean([m[metric] for m in table[task]["GB"]]), len(table[task]["GA"]), len(table[task]["GB"])])
    if options.json:
        json.dump({"summary": summary, "effects": rows, "sensitivity": sensitivity, "tasks": sorted(table)}, open(options.json, "w"), indent=1)


if __name__ == "__main__":
    main()
