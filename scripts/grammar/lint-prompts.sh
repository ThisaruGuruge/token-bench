#!/usr/bin/env bash
# Wording lint (design §4.1): no prompt may contain a word that is a grammar
# token in only one grammar. Prints the prompts it checked and fails on a hit.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
prompts="$(go run ./cmd/dq-prompts)"
banned='\b(methods?|functions?|resources?|packages?|funcs|buckets?)\b|(^|[[:space:]])-p\b'
count="$(printf '%s\n' "$prompts" | grep -c '^=== dq-' || true)"
if hits="$(printf '%s\n' "$prompts" | grep -inE "$banned")"; then
  printf 'LINT FAIL: grammar tokens in prompts:\n%s\n' "$hits"
  exit 1
fi
echo "LINT PASS: $count prompts, no grammar-specific word (banned: method, function, resource, package, -p, funcs, bucket)"
