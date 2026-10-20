#!/usr/bin/env bash
# $0 end-to-end of the integration scenario: the runner drives the fake-int
# image (a fake claude that copies the reference solution in) on a per-run
# network with Kafka and the mocks, then grades it.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GRAMMAR_DIR="${GRAMMAR_DIR:-$HOME/Projects/Ballerina/cli-improvements/benchmark/grammar}"
RUNNER="${RUNNER:-$ROOT/token-bench}"
work="$GRAMMAR_DIR/results/e2e-fake-int-$(date +%Y%m%dT%H%M%S)"
mkdir -p "$work/arms/hints"
cp "$ROOT/arms/hints/discover-neutral.md" "$work/arms/hints/"
cat > "$work/arms/GFAKEI.json" <<EOF
{ "id": "GFAKEI", "label": "fake claude, integration", "discoverVisible": true, "systemPromptFile": "hints/discover-neutral.md",
  "container": { "image": "token-bench-discover:fake-int", "pluginDir": "/opt/bench-plugin" }, "callLog": true,
  "cacheSeed": "$GRAMMAR_DIR/cache-seed/bal-discover", "disallowedTools": ["WebSearch", "WebFetch"], "grammar": "A" }
EOF
key_file="$(mktemp)"
trap 'rm -f "$key_file"' EXIT
printf 'fake-int-key-%s' "$RANDOM" > "$key_file"
set +e
TOKEN_BENCH_API_KEY_HELPER="cat $key_file" "$RUNNER" lead-routing ballerina claude --arm GFAKEI --arms-dir "$work/arms" \
  --model sonnet --max-budget-usd 0.01 --target "$work/run" > "$work/runner.log" 2>&1
set -e
python3 - "$work" <<'PY'
import glob, json, subprocess, sys
work = sys.argv[1]
[path] = glob.glob(f"{work}/run/*/result.json")
value = json.load(open(path))
report = None
for line in open(path.replace("result.json", "claude-stream.jsonl")):
    event = json.loads(line)
    if event.get("type") == "result":
        report = json.loads(event["result"].removeprefix("FAKE-CHECKS "))
answer = value.get("answer") or {}
print("passed", value["passed"], "infra", value.get("infraFailure"), "error", (value.get("error") or "")[:200])
print("checks", answer.get("correct"), "/", answer.get("total"), [ (f["field"], f["got"]) for f in answer.get("fields", []) if not f["correct"]])
print("fake report", report)
networks = subprocess.run(["docker", "network", "ls", "--format", "{{.Name}}"], capture_output=True, text=True).stdout.split()
containers = subprocess.run(["docker", "ps", "-a", "--format", "{{.Names}}"], capture_output=True, text=True).stdout.split()
left = [n for n in networks if n.startswith("tb-net-")] + [c for c in containers if c.startswith("tb-")]
ok = (value["passed"] and report and all(v == 200 for v in report["reach"].values()) and report["skillNeutral"]
      and report["build"] == 0 and report["discover"] == 0 and not left)
print("leftovers", left)
print("E2E-INT PASS" if ok else "E2E-INT FAIL")
sys.exit(0 if ok else 1)
PY
