#!/usr/bin/env bash
# Builds the grammar benchmark's container images.
#
#   build-images.sh base                      Linux Ballerina + Claude Code + the bal-calllog wrapper
#   build-images.sh arm <GA|GB> <worktree>    the base plus that worktree's freshly built bal discover
#   build-images.sh fake                      the base with a fake claude, for the $0 end-to-end test
#
# The arm build refuses a dirty worktree and a bala older than its HEAD, uses
# the worktree's built bala (BUILD_BALA=1 rebuilds it with Gradle, without
# publishing), and installs it inside the image, never in the host's ~/.ballerina. It records the image in $GRAMMAR_DIR/images/<arm>.json.
#
# Environment: GRAMMAR_DIR (default ~/Projects/Ballerina/cli-improvements/benchmark/grammar),
# BAL_VERSION (default 2201.14.0), CLAUDE_CODE_VERSION (default: the host's claude --version).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GRAMMAR_DIR="${GRAMMAR_DIR:-$HOME/Projects/Ballerina/cli-improvements/benchmark/grammar}"
BAL_VERSION="${BAL_VERSION:-2201.14.0}"
CLAUDE_CODE_VERSION="${CLAUDE_CODE_VERSION:-$(claude --version | awk '{print $1}')}"
BASE_IMAGE="token-bench-discover-base:${BAL_VERSION}-cc${CLAUDE_CODE_VERSION}"
BALA_RELATIVE="bal-tool/build/bal_build_target/bala/ballerina-tool_discover-java21-0.1.0.bala"

case "$(docker version --format '{{.Server.Arch}}')" in
  arm64|aarch64) GOARCH=arm64; DEB_ARCH=linux-arm-x64 ;;
  amd64|x86_64)  GOARCH=amd64; DEB_ARCH=linux-x64 ;;
  *) echo "unsupported docker architecture" >&2; exit 1 ;;
esac

context="$(mktemp -d)"
trap 'rm -rf "$context"' EXIT

build_base() {
  local deb="$GRAMMAR_DIR/downloads/ballerina-${BAL_VERSION}-swan-lake-${DEB_ARCH}.deb"
  if [[ ! -s "$deb" ]]; then
    mkdir -p "$(dirname "$deb")"
    curl -fL --retry 3 -o "$deb.part" "https://dist.ballerina.io/downloads/${BAL_VERSION}/$(basename "$deb")"
    mv "$deb.part" "$deb"
  fi
  cp "$deb" "$context/ballerina.deb"
  (cd "$ROOT" && GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 go build -trimpath -o "$context/bal-calllog" ./cmd/bal-calllog)
  cp "$ROOT/docker/discover-arm/entrypoint.sh" "$context/entrypoint.sh"
  docker build --platform "linux/$GOARCH" -f "$ROOT/docker/discover-arm/Dockerfile.base" \
    --build-arg "CLAUDE_CODE_VERSION=$CLAUDE_CODE_VERSION" --build-arg "BAL_VERSION=$BAL_VERSION" \
    -t "$BASE_IMAGE" "$context"
  echo "built $BASE_IMAGE"
}

build_arm() {
  local arm="$1" worktree="$2"
  [[ "$arm" =~ ^G[AB]$ ]] || { echo "arm must be GA or GB" >&2; exit 2; }
  docker image inspect "$BASE_IMAGE" >/dev/null 2>&1 || { echo "build the base first: $0 base" >&2; exit 2; }
  if [[ -n "$(git -C "$worktree" status --porcelain --untracked-files=no)" ]]; then
    echo "$worktree has uncommitted changes; build from a committed HEAD" >&2
    exit 2
  fi
  local branch commit
  branch="$(git -C "$worktree" branch --show-current)"
  commit="$(git -C "$worktree" rev-parse HEAD)"
  if [[ "${BUILD_BALA:-0}" == "1" ]]; then
    rm -f "$worktree/$BALA_RELATIVE"
    (cd "$worktree" && ./gradlew :bal-tool:build -x test --rerun-tasks --no-build-cache -q)
  fi
  [[ -s "$worktree/$BALA_RELATIVE" ]] || { echo "no bala at $worktree/$BALA_RELATIVE (BUILD_BALA=1 builds it)" >&2; exit 1; }
  # A bala older than HEAD's commit cannot have been built from it.
  if (( $(stat -f %m "$worktree/$BALA_RELATIVE") < $(git -C "$worktree" log -1 --format=%ct) )); then
    echo "the bala is older than HEAD ($commit); rebuild it (BUILD_BALA=1)" >&2
    exit 2
  fi
  cp "$worktree/$BALA_RELATIVE" "$context/tool_discover.bala"
  local sha
  sha="$(shasum -a 256 "$context/tool_discover.bala" | awk '{print $1}')"
  docker build --platform "linux/$GOARCH" -f "$ROOT/docker/discover-arm/Dockerfile.arm" \
    --build-arg "BASE_IMAGE=$BASE_IMAGE" --build-arg "ARM=$arm" --build-arg "BRANCH=$branch" \
    --build-arg "COMMIT=$commit" --build-arg "BALA_SHA256=$sha" \
    -t "token-bench-discover:$arm" "$context"
  mkdir -p "$GRAMMAR_DIR/images"
  local id
  id="$(docker image inspect --format '{{.Id}}' "token-bench-discover:$arm")"
  printf '{"arm":"%s","image":"token-bench-discover:%s","imageId":"%s","base":"%s","branch":"%s","commit":"%s","balaSha256":"%s","built":"%s"}\n' \
    "$arm" "$arm" "$id" "$BASE_IMAGE" "$branch" "$commit" "$sha" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$GRAMMAR_DIR/images/$arm.json"
  echo "built token-bench-discover:$arm from $branch@$commit (bala $sha)"
}

build_fake() {
  docker image inspect "$BASE_IMAGE" >/dev/null 2>&1 || { echo "build the base first: $0 base" >&2; exit 2; }
  cp "$ROOT/scripts/grammar/fake-claude.sh" "$context/fake-claude.sh"
  cat > "$context/Dockerfile" <<EOF
FROM $BASE_IMAGE
USER root
RUN rm -f /usr/local/bin/claude
COPY fake-claude.sh /usr/local/bin/claude
RUN chmod 755 /usr/local/bin/claude
USER node
LABEL org.token-bench.arm="fake"
EOF
  docker build --platform "linux/$GOARCH" -t token-bench-discover:fake "$context"
}

case "${1:-}" in
  base) build_base ;;
  arm) [[ $# -eq 3 ]] || { echo "usage: $0 arm <GA|GB> <worktree>" >&2; exit 2; }; build_arm "$2" "$3" ;;
  fake) build_fake ;;
  *) sed -n '2,13p' "$0"; exit 2 ;;
esac
