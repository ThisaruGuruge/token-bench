#!/usr/bin/env bash
# $0 check that claude inside a benchmark container authenticates with the key
# the harness hands in, and nothing else: a bogus key must end the run on an
# HTTP 401 with zero cost (an infrastructure failure), with no fallback to any
# login. Uses the base image (real Claude Code, no discover).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GRAMMAR_DIR="${GRAMMAR_DIR:-$HOME/Projects/Ballerina/cli-improvements/benchmark/grammar}"
IMAGE="${1:-token-bench-discover-base:2201.14.0-cc$(claude --version | awk '{print $1}')}"
work="$GRAMMAR_DIR/results/bogus-key-$(date +%Y%m%dT%H%M%S)"
mkdir -p "$work/arms/hints"
cp "$ROOT/arms/hints/discover-neutral.md" "$work/arms/hints/"
printf '{"id":"GBOGUS","label":"bogus key check","discoverVisible":true,"systemPromptFile":"hints/discover-neutral.md","container":{"image":"%s"},"disallowedTools":["WebSearch","WebFetch"]}\n' "$IMAGE" > "$work/arms/GBOGUS.json"
key_file="$(mktemp)"
trap 'rm -f "$key_file"' EXIT
printf 'sk-ant-api03-bogus-%s' "$RANDOM$RANDOM" > "$key_file"
(cd "$ROOT" && make -s token-bench)
set +e
TOKEN_BENCH_API_KEY_HELPER="cat $key_file" "$ROOT/token-bench" dq-D01 ballerina claude --arm GBOGUS \
  --arms-dir "$work/arms" --model haiku --max-budget-usd 0.01 --target "$work/run" > "$work/runner.log" 2>&1
set -e
python3 - "$work" <<'PY'
import glob, json, sys
work = sys.argv[1]
[path] = glob.glob(f"{work}/run/*/result.json")
value = json.load(open(path))
stream = [json.loads(line) for line in open(path.replace("result.json", "claude-stream.jsonl")) if line.strip()]
results = [event for event in stream if event.get("type") == "result"]
last = results[-1] if results else {}
print(json.dumps({"infraFailure": value.get("infraFailure"), "error": value.get("error", "")[:200],
                  "api_error_status": last.get("api_error_status"), "is_error": last.get("is_error"),
                  "total_cost_usd": last.get("total_cost_usd"), "reportedCostUsd": value.get("reportedCostUsd"),
                  "durationSeconds": round(value.get("durationSeconds", 0))}, indent=1))
ok = value.get("infraFailure") and last.get("api_error_status") == 401 and (last.get("total_cost_usd") or 0) == 0
print("PASS: 401 with zero cost, no fallback credential" if ok else "FAIL")
sys.exit(0 if ok else 1)
PY
