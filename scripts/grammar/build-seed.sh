#!/usr/bin/env bash
# Builds the shared, pre-warmed discover cache seed: every command of
# seed-commands.json run through GA and then GB into ONE cache directory, so
# both arms start every run from the same bytes. Central search is never
# cached, so the search commands only warm nothing; that is expected.
#
#   build-seed.sh            writes $GRAMMAR_DIR/cache-seed/bal-discover (replacing it)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GRAMMAR_DIR="${GRAMMAR_DIR:-$HOME/Projects/Ballerina/cli-improvements/benchmark/grammar}"
# SEED_NAME selects another seed directory (cache-seed-int for the integration
# arms); EXTRA_COMMANDS adds a second commands file to the base list.
seed_root="$GRAMMAR_DIR/${SEED_NAME:-cache-seed}"
seed="$seed_root/bal-discover"
out="$seed_root/build-log"
commands="$HERE/seed-commands.json"
if [[ -n "${EXTRA_COMMANDS:-}" ]]; then
  commands="$seed_root/commands.json"
  mkdir -p "$seed_root"
  python3 -c 'import json,sys; json.dump(json.load(open(sys.argv[1]))+json.load(open(sys.argv[2])), open(sys.argv[3],"w"))' "$HERE/seed-commands.json" "$EXTRA_COMMANDS" "$commands"
fi
rm -rf "$seed" "$out"
mkdir -p "$seed" "$out"
chmod 777 "$seed" "$out"
for arm in GA GB; do
  grammar="${arm#G}"
  platform="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "token-bench-discover:$arm")"
  docker run --rm --platform "$platform" --entrypoint node \
    -v "$HERE:/s:ro" -v "$(dirname "$commands"):/c:ro" -v "$seed:/seed" -v "$out:/out" -e BAL_DISCOVER_CACHE_DIR=/seed \
    "token-bench-discover:$arm" /s/run-commands.js "/c/$(basename "$commands")" "$grammar" /out
done
python3 - "$out" <<'PY'
import json, sys
bad = []
for grammar in "AB":
    for run in json.load(open(f"{sys.argv[1]}/{grammar}.json")):
        if run["exit"] != 0:
            bad.append(f"{grammar} {run['id']} exit {run['exit']}: {run['stderr'][:160]}")
print("\n".join(bad) if bad else "every seed command answered (exit 0) in both grammars")
sys.exit(1 if bad else 0)
PY
find "$seed" -type f | wc -l | xargs echo "seed files:"
