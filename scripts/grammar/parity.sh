#!/usr/bin/env bash
# $0 parity preflight (design §2.6): runs the 30 equivalent command pairs of
# parity-pairs.json in the GA and GB images, each from its own re-stamped copy
# of the shared cache seed, and asserts the same exit code, the same failure
# kind, and the same answer content once printed commands are removed.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GRAMMAR_DIR="${GRAMMAR_DIR:-$HOME/Projects/Ballerina/cli-improvements/benchmark/grammar}"
seed="$GRAMMAR_DIR/cache-seed/bal-discover"
work="$GRAMMAR_DIR/preflight/parity-$(date +%Y%m%dT%H%M%S)"
mkdir -p "$work/out"
chmod 777 "$work/out"
for arm in GA GB; do
  cache="$work/cache-$arm"
  python3 "$HERE/copy-seed.py" "$seed" "$cache"
  platform="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "token-bench-discover:$arm")"
  docker run --rm --platform "$platform" --entrypoint node \
    -v "$HERE:/s:ro" -v "$cache:/cache" -v "$work/out:/out" -e BAL_DISCOVER_CACHE_DIR=/cache \
    "token-bench-discover:$arm" /s/run-commands.js /s/parity-pairs.json "${arm#G}" /out
done
python3 "$HERE/compare-parity.py" "$HERE/parity-pairs.json" "$work" "$seed" | tee "$work/verdict.txt"
