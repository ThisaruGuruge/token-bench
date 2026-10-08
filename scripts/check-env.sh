#!/usr/bin/env bash
# Verifies the benchmark environment and prints a pinned-versions block for the report.
# Spends no tokens: it only runs --version style commands. Exit status is 1 when a
# hard requirement is missing; warnings (for example a different bal version) do not fail.
#
# Environment overrides:
#   EXPECTED_BAL_VERSION   default 2201.13.6
#   TOKEN_BENCH_SKILLS_REPO  skills checkout (default: the pinned reference snapshot)
#   EXPECTED_SKILLS_COMMIT   default a87ce03f816b19c5603632a581f0d353a264ec22

set -u

EXPECTED_BAL_VERSION="${EXPECTED_BAL_VERSION:-2201.13.6}"
EXPECTED_SKILLS_COMMIT="${EXPECTED_SKILLS_COMMIT:-a87ce03f816b19c5603632a581f0d353a264ec22}"
SKILLS_REPO="${TOKEN_BENCH_SKILLS_REPO:-$HOME/Projects/Ballerina/cli-improvements/benchmark/reference/ballerina-skills}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

failures=0
warnings=0
pins=()

ok()   { printf '  ok    %s\n' "$1"; }
warn() { printf '  WARN  %s\n' "$1"; warnings=$((warnings + 1)); }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
pin()  { pins+=("$1: $2"); }

echo "Checking benchmark environment"

# bal
if command -v bal >/dev/null 2>&1; then
  bal_version="$(bal version 2>/dev/null | head -n 1)"
  if [[ "$bal_version" == *"$EXPECTED_BAL_VERSION"* ]]; then
    ok "bal: $bal_version"
  else
    warn "bal is '$bal_version', expected $EXPECTED_BAL_VERSION"
  fi
  pin "Ballerina" "${bal_version:-unknown}"
  discover_row="$(bal tool list 2>/dev/null | awk -F'|' '$2 ~ /^ *discover *$/ {gsub(/^ +| +$/, "", $3); print $3}')"
  if [[ -n "$discover_row" ]] && bal discover --help >/dev/null 2>&1; then
    ok "bal discover --help works (version $discover_row)"
    pin "bal discover" "$discover_row"
  else
    fail "bal discover is not installed or --help fails (arms C and D need it); see the discover-tool README: bal tool pull discover"
    pin "bal discover" "not installed"
  fi
else
  fail "bal not found on PATH"
  pin "Ballerina" "not installed"
fi

# docker / colima
if command -v docker >/dev/null 2>&1; then
  if docker info >/dev/null 2>&1; then
    ok "docker daemon is running"
    pin "Docker" "$(docker --version 2>/dev/null)"
  else
    fail "docker is installed but the daemon is not running (checkout-sync and four other scenarios need it)"
    if command -v colima >/dev/null 2>&1; then
      echo "        hint: colima start"
    fi
  fi
else
  fail "docker not found on PATH"
fi

# go
if command -v go >/dev/null 2>&1; then
  go_version="$(go version)"
  ok "$go_version"
  pin "Go" "$go_version"
else
  fail "go not found on PATH"
fi

# claude (only --version: no session is started)
if command -v claude >/dev/null 2>&1; then
  claude_version="$(claude --version 2>/dev/null | head -n 1)"
  ok "claude: $claude_version"
  pin "Claude Code" "$claude_version"
else
  fail "claude not found on PATH"
fi

# node + jq (arm E: MCP server and hooks)
if command -v node >/dev/null 2>&1; then
  node_version="$(node --version 2>/dev/null)"
  node_major="${node_version#v}"; node_major="${node_major%%.*}"
  if [[ "$node_major" =~ ^[0-9]+$ ]] && [[ "$node_major" -ge 18 ]]; then
    ok "node $node_version (arm E MCP server needs 18+)"
  else
    fail "node '$node_version' is older than 18 (arm E MCP server)"
  fi
  pin "Node" "$node_version"
else
  fail "node not found on PATH (arm E's library MCP server is run with node)"
fi
if command -v jq >/dev/null 2>&1; then
  ok "jq present (arm E hooks use it)"
else
  warn "jq not found; arm E's PreToolUse hook will silently do nothing"
fi
if command -v patch >/dev/null 2>&1; then
  ok "patch present (arm D stages a patched skill copy)"
else
  fail "patch not found (arm D)"
fi

# Benchmark API key: token-bench's default credential source is this Keychain
# item. Only its presence is checked; the secret is never read here.
if [ -n "${TOKEN_BENCH_API_KEY_HELPER:-}" ]; then
  ok "API key helper overridden by TOKEN_BENCH_API_KEY_HELPER (not checked here; token-bench checks it before the first run)"
elif command -v security >/dev/null 2>&1 && security find-generic-password -s token-bench-anthropic-api-key >/dev/null 2>&1; then
  ok "benchmark API key stored in the Keychain (token-bench-anthropic-api-key)"
else
  fail "benchmark API key not in the Keychain; store it with: security add-generic-password -a \"\$USER\" -s token-bench-anthropic-api-key -w"
fi

# skills repository
if [[ -d "$SKILLS_REPO" ]]; then
  commit=""
  source=""
  if [[ -d "$SKILLS_REPO/.git" ]]; then
    commit="$(git -C "$SKILLS_REPO" rev-parse HEAD 2>/dev/null)"
    source="git"
  elif [[ -f "$(dirname "$SKILLS_REPO")/SOURCES.txt" ]]; then
    commit="$(awk -v name="$(basename "$SKILLS_REPO")" '$1 == name {print $2}' "$(dirname "$SKILLS_REPO")/SOURCES.txt")"
    source="SOURCES.txt"
  fi
  if [[ -z "$commit" ]]; then
    warn "skills repository $SKILLS_REPO has no git metadata or SOURCES.txt entry; commit unknown"
    commit="unknown"
  elif [[ "$commit" == "$EXPECTED_SKILLS_COMMIT"* || "$EXPECTED_SKILLS_COMMIT" == "$commit"* ]]; then
    ok "skills repository $SKILLS_REPO at $commit ($source)"
  else
    warn "skills repository is at $commit ($source), arms were designed against $EXPECTED_SKILLS_COMMIT"
  fi
  pin "Skills repository" "$SKILLS_REPO @ $commit"
else
  fail "skills repository not found at $SKILLS_REPO (set TOKEN_BENCH_SKILLS_REPO)"
fi

# the fork itself
if git -C "$ROOT" rev-parse HEAD >/dev/null 2>&1; then
  fork_commit="$(git -C "$ROOT" rev-parse --short HEAD)"
  if [[ -n "$(git -C "$ROOT" status --porcelain 2>/dev/null)" ]]; then
    fork_commit="$fork_commit (uncommitted changes)"
    warn "token-bench has uncommitted changes; the pinned commit below does not describe the code that will run"
  fi
  pin "token-bench" "$fork_commit ($(git -C "$ROOT" rev-parse --abbrev-ref HEAD))"
fi
for binary in token-bench token-bench-matrix token-bench-report; do
  if [[ -x "$ROOT/$binary" ]]; then
    ok "$binary is built"
  else
    warn "$binary is not built (make all)"
  fi
done

echo
echo "Pinned versions (paste into the report):"
echo '```'
for line in "${pins[@]}"; do
  echo "$line"
done
echo '```'
echo
echo "$failures failure(s), $warnings warning(s)"
[[ "$failures" -eq 0 ]]
