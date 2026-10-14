#!/usr/bin/env bash
# $0 end-to-end test of the container path: the real matrix runner and runner
# drive the fake-claude image (build-images.sh fake) with a fake key through
# the same plumbing a paid run uses, then the results are checked.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GRAMMAR_DIR="${GRAMMAR_DIR:-$HOME/Projects/Ballerina/cli-improvements/benchmark/grammar}"
FAKE_KEY="fake-e2e-key-$RANDOM$RANDOM"
key_file="$(mktemp)"
trap 'rm -f "$key_file"' EXIT
printf '%s' "$FAKE_KEY" > "$key_file"
work="$GRAMMAR_DIR/results/e2e-fake-$(date +%Y%m%dT%H%M%S)"
arms="$work/arms"
seed="$work/seed/bal-discover"
mkdir -p "$arms/hints" "$seed/v2/latest/central/ballerinax" "$seed/v2/docs/central/ballerinax/kafka"
cp "$ROOT/arms/hints/discover-neutral.md" "$arms/hints/"
printf '{"version":"4.6.6","atMs":1}' > "$seed/v2/latest/central/ballerinax/kafka.json"
printf '{}' > "$seed/v2/docs/central/ballerinax/kafka/4.6.6.json"
cat > "$arms/GFAKE.json" <<EOF
{ "id": "GFAKE", "label": "fake claude", "discoverVisible": true, "systemPromptFile": "hints/discover-neutral.md",
  "container": { "image": "token-bench-discover:fake" }, "callLog": true, "cacheSeed": "$seed",
  "disallowedTools": ["WebSearch", "WebFetch"], "grammar": "A" }
EOF
(cd "$ROOT" && make -s token-bench token-bench-matrix)
TOKEN_BENCH_API_KEY_HELPER="cat $key_file" "$ROOT/token-bench-matrix" --runner "$ROOT/token-bench" \
  --arms-dir "$arms" --arms GFAKE --scenarios dq-D01,dq-D02 --runs 1 --model haiku --max-budget-usd 0.01 \
  --target "$work/results" --seed 7

python3 - "$work" "$FAKE_KEY" <<'EOF'
import glob, json, os, sys
work, key = sys.argv[1], sys.argv[2]
failures = []
def check(ok, what):
    print(("  ok    " if ok else "  FAIL  ") + what)
    if not ok:
        failures.append(what)
results = {}
for path in glob.glob(f"{work}/results/GFAKE/*/run-01/*/result.json"):
    value = json.load(open(path))
    results[value["task"]] = (value, os.path.dirname(path))
check(set(results) == {"dq-D01", "dq-D02"}, "both cells wrote a result")
for task, (value, directory) in sorted(results.items()):
    check(not value.get("infraFailure"), f"{task}: not an infrastructure failure ({value.get('error')})")
    check(value["schemaVersion"] == 6 and value["arm"] == "GFAKE", f"{task}: schema 6, arm recorded")
    check(value["passed"] == (task == "dq-D01"), f"{task}: graded from answer.json (passed={value['passed']})")
    check(value["answer"]["present"], f"{task}: answer present")
    discover = value.get("discover") or {}
    check(discover.get("calls") == 2 and discover.get("otherBalCalls") == 1, f"{task}: call log has 2 discover + 1 other bal call")
    check(discover.get("liveFetches") == 0, f"{task}: no cache files beyond the seed")
    env = value["environment"]
    check(env["container"]["image"] == "token-bench-discover:fake" and env["container"]["platform"], f"{task}: image and platform recorded")
    check(env["cacheSeed"]["files"] == 2, f"{task}: seed recorded")
    check(env["disallowedTools"] == ["WebSearch", "WebFetch"], f"{task}: disallowed tools recorded")
    check(os.path.exists(f"{directory}/calls/calls.jsonl"), f"{task}: call log copied into the result")
    stream = open(f"{directory}/claude-stream.jsonl").read()
    report = None
    for line in stream.splitlines():
        event = json.loads(line)
        if event.get("type") == "result":
            report = json.loads(event["result"].removeprefix("FAKE-CHECKS "))
    check(report is not None, f"{task}: fake claude reported its checks")
    if report:
        check(report["keyLength"] == len(key), f"{task}: in-container apiKeyHelper returns the key")
        check(not report["envHasKey"], f"{task}: no credential variable in the agent's environment")
        check(".claude" not in report["home"], f"{task}: no Claude login or settings in the container home")
        check(report["cwd"] == "/work" and report["cwdEntries"] == [], f"{task}: working dir is /work and starts empty")
        check("WebSearch" in report["disallowed"] and "WebFetch" in report["disallowed"], f"{task}: web tools disallowed")
        statuses = [call["status"] for call in report["calls"]]
        check(statuses[0] == 0, f"{task}: bal works through the wrapper (statuses {statuses})")
leaks = []
for directory, _, files in os.walk(work):
    for name in files:
        try:
            if key.encode() in open(os.path.join(directory, name), "rb").read():
                leaks.append(os.path.join(directory, name))
        except OSError:
            pass
check(not leaks, f"the key is in no file under the results ({leaks})")
check(not glob.glob(f"{work}/results/GFAKE/*/run-01/.token-bench-private/*"), "private run directories cleaned up")
sys.exit(1 if failures else 0)
EOF
echo "e2e results: $work"
