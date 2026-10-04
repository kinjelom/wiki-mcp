#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# wiki-mcp - build and publish the server image
#
# The image carries the binary and nothing else: no documentation, no shell,
# no package manager. It reads /etc/wiki-mcp/config.yml and keeps its sessions
# in /var/lib/wiki-mcp - see the Deployment section of README.md.
#
# Two ways to build, because they need different things:
#
#   default       docker build, this machine's architecture only.
#   --multi-arch  docker buildx, every platform in IMAGE_PLATFORMS, in one
#                 manifest list. A multi-arch build cannot stay in the local
#                 image store, so it implies --push.
#
# Usage:
#   scripts/image.sh                        # build locally, tag with git describe
#   scripts/image.sh --version 1.2.0        # tag as a release
#   scripts/image.sh --push                 # build, then push version + latest
#   scripts/image.sh --version 1.2.0 --multi-arch --push
#
# Credentials come from the environment, never from a file or an argument:
#   REGISTRY_USER / REGISTRY_PASSWORD, or GITHUB_TOKEN for ghcr.io.
# ---------------------------------------------------------------------------

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

load_config

VERSION=""
PUSH=0
MULTI_ARCH=0

while [ $# -gt 0 ]; do
  case "$1" in
    --version)     VERSION="${2:?--version needs a value}"; shift ;;
    --push)        PUSH=1 ;;
    --multi-arch)  MULTI_ARCH=1; PUSH=1 ;;
    -h|--help)     usage "${BASH_SOURCE[0]}"; exit 0 ;;
    *)             die "unknown argument: $1" ;;
  esac
  shift
done

resolve_docker
[ -n "$VERSION" ] || VERSION="$(describe_version)"

IMAGE_REF="$IMAGE_REPO:$VERSION"

step "Image $IMAGE_REF"
log "engine     : $DOCKER"
log "registry   : ${REGISTRY:-<docker hub>}"
[ -n "$IMAGE_LATEST_TAG" ] && log "extra tag  : $IMAGE_REPO:$IMAGE_LATEST_TAG"

# --- Authentication ---------------------------------------------------------
#
# Only when something is going to be pushed. A local build needs no login, and
# asking for one would make a build fail on a machine that has no credentials.
registry_login() {
  local host="${REGISTRY:-docker.io}"
  local user="${REGISTRY_USER:-}"
  local password="${REGISTRY_PASSWORD:-}"

  # ghcr.io authenticates with a GitHub token; the username is the account.
  if [ -z "$password" ] && [ "$host" = "ghcr.io" ] && [ -n "${GITHUB_TOKEN:-}" ]; then
    user="${user:-$GITHUB_OWNER}"
    password="$GITHUB_TOKEN"
  fi

  if [ -z "$user" ] || [ -z "$password" ]; then
    warn "no credentials in the environment - assuming '$DOCKER' is already logged in to $host"
    return 0
  fi

  log "authenticating to $host as $user"
  # The password goes in on stdin. As an argument it would be visible in the
  # process list of every other user on the machine.
  printf '%s' "$password" | "$DOCKER" login "$host" --username "$user" --password-stdin >/dev/null \
    || die "login to $host failed"
  ok "authenticated"
}

[ "$PUSH" -eq 1 ] && registry_login

tags=(--tag "$IMAGE_REF")
if [ -n "$IMAGE_LATEST_TAG" ]; then
  tags+=(--tag "$IMAGE_REPO:$IMAGE_LATEST_TAG")
fi

# .git is not in the build context (.dockerignore), so what the binary reports
# about its origin comes in as build arguments.
build_args=(
  --build-arg "VERSION=$VERSION"
  --build-arg "REVISION=$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  --build-arg "BRANCH=$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
  --build-arg "SOURCE_URL=https://github.com/$GITHUB_OWNER/$GITHUB_REPO"
  --build-arg "GO_IMAGE=${GO_BASE_IMAGE:-docker.io/library/golang:1.26-alpine}"
  --build-arg "RUNTIME_IMAGE=${RUNTIME_BASE_IMAGE:-gcr.io/distroless/static-debian12:nonroot}"
)

cd "$REPO_ROOT"

if [ "$MULTI_ARCH" -eq 1 ]; then
  platforms="$(printf '%s' "$IMAGE_PLATFORMS" | tr ' ' ',')"
  log "platforms  : $platforms"
  "$DOCKER" buildx version >/dev/null 2>&1 \
    || die "$DOCKER buildx is required for --multi-arch
     Install the buildx plugin, or drop --multi-arch and build one architecture."
  # A manifest list has no single local image to load, so buildx pushes
  # directly. That is why --multi-arch implies --push.
  "$DOCKER" buildx build --platform "$platforms" "${build_args[@]}" "${tags[@]}" --push .
  ok "pushed $IMAGE_REF"
  exit 0
fi

"$DOCKER" build "${build_args[@]}" "${tags[@]}" .
ok "built $IMAGE_REF"

# The image is distroless, so the binary is what answers the question of what
# it is. This also proves the entrypoint runs at all.
"$DOCKER" run --rm "$IMAGE_REF" --version

if [ "$PUSH" -eq 1 ]; then
  step "Push"
  "$DOCKER" push "$IMAGE_REF"
  ok "pushed $IMAGE_REF"
  if [ -n "$IMAGE_LATEST_TAG" ]; then
    "$DOCKER" push "$IMAGE_REPO:$IMAGE_LATEST_TAG"
    ok "pushed $IMAGE_REPO:$IMAGE_LATEST_TAG"
  fi
fi
