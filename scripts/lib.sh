#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# wiki-mcp - shared shell helpers
#
# Sourced by every scripts/*.sh. Never executed directly.
#   . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
# ---------------------------------------------------------------------------

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_FILE="${CONFIG_FILE:-$REPO_ROOT/release.conf}"
BIN_DIR="${BIN_DIR:-$REPO_ROOT/bin}"
DIST_DIR="${DIST_DIR:-$REPO_ROOT/dist}"

# --- Output ----------------------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_INFO=$'\033[36m'; C_OK=$'\033[32m'
  C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_BOLD=$'\033[1m'
else
  C_RESET=''; C_INFO=''; C_OK=''; C_WARN=''; C_ERR=''; C_BOLD=''
fi

log()   { printf '%s==>%s %s\n' "$C_INFO" "$C_RESET" "$*"; }
ok()    { printf '%s  OK%s %s\n' "$C_OK" "$C_RESET" "$*"; }
warn()  { printf '%sWARN%s %s\n' "$C_WARN" "$C_RESET" "$*" >&2; }
die()   { printf '%sERROR%s %s\n' "$C_ERR" "$C_RESET" "$*" >&2; exit 1; }
step()  { printf '\n%s%s%s\n' "$C_BOLD" "$*" "$C_RESET"; }

# Print the header comment block of the calling script as its --help text.
# Stops at the first line that is not a comment, and drops the box rules, so
# the help never bleeds into the code below it.
usage() {
  local file="${1:-${BASH_SOURCE[1]}}"
  awk '
    NR == 1 && /^#!/ { next }
    /^#/             { sub(/^# ?/, ""); if ($0 !~ /^-+$/) print; next }
                     { exit }
  ' "$file"
}

# --- Configuration ---------------------------------------------------------

# Parse release.conf into shell variables.
#
# The file is parsed line by line rather than sourced: values are taken
# literally, so nothing in release.conf can execute code. Values already
# present in the environment win, which is what lets CI - or a fork - override
# a single setting without editing a committed file.
load_config() {
  [ -f "$CONFIG_FILE" ] || die "configuration file not found: $CONFIG_FILE"

  local line key value lineno=0
  while IFS= read -r line || [ -n "$line" ]; do
    lineno=$((lineno + 1))
    line="${line%$'\r'}"                        # tolerate CRLF checkouts
    case "$line" in ''|'#'*) continue ;; esac
    case "$line" in
      *=*) ;;
      *) die "$CONFIG_FILE:$lineno: expected KEY=VALUE, got: $line" ;;
    esac
    key="${line%%=*}"
    value="${line#*=}"
    key="$(printf '%s' "$key" | tr -d '[:space:]')"
    value="${value#"${value%%[![:space:]]*}"}"  # ltrim
    value="${value%"${value##*[![:space:]]}"}"  # rtrim
    case "$key" in
      ''|*[!A-Za-z0-9_]*) die "$CONFIG_FILE:$lineno: invalid key: $key" ;;
    esac
    # The literal `none` means "not set", so every key can keep a value and
    # the file never has a dangling right-hand side.
    if [ "$value" = "none" ]; then
      value=""
    fi
    if [ -z "${!key:-}" ]; then
      printf -v "$key" '%s' "$value"
    fi
    export "$key"
  done < "$CONFIG_FILE"

  : "${BINARY_NAME:?BINARY_NAME missing from $CONFIG_FILE}"
  : "${IMAGE_NAME:?IMAGE_NAME missing from $CONFIG_FILE}"
  : "${GITHUB_OWNER:?GITHUB_OWNER missing from $CONFIG_FILE}"
  : "${GITHUB_REPO:?GITHUB_REPO missing from $CONFIG_FILE}"
  : "${RELEASE_PLATFORMS:=linux/amd64}"
  : "${IMAGE_PLATFORMS:=linux/amd64}"
  : "${IMAGE_LATEST_TAG:=}"
  : "${DEFAULT_BRANCH:=main}"

  IMAGE_REPO="$(compose_repo)"
  export IMAGE_REPO
}

# registry/namespace/name, skipping the parts that are empty.
compose_repo() {
  local repo=""
  if [ -n "${REGISTRY:-}" ]; then repo="$REGISTRY/"; fi
  if [ -n "${IMAGE_NAMESPACE:-}" ]; then repo="$repo$IMAGE_NAMESPACE/"; fi
  printf '%s%s' "$repo" "$IMAGE_NAME"
}

# --- Tooling ---------------------------------------------------------------

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1${2:+ ($2)}"
}

# Resolve the container CLI once. DOCKER can be preset to force a choice.
resolve_docker() {
  if [ -n "${DOCKER:-}" ]; then
    command -v "$DOCKER" >/dev/null 2>&1 || die "DOCKER is set to '$DOCKER' but it is not on PATH"
  elif command -v docker >/dev/null 2>&1; then
    DOCKER=docker
  elif command -v podman >/dev/null 2>&1; then
    DOCKER=podman
  else
    die "neither docker nor podman found on PATH"
  fi
  export DOCKER
}

# --- Registry credentials --------------------------------------------------
#
# Answered before anything is published, not when the push runs. The image push
# is the last step of a release, and by then the branch and the tag are already
# on the remote - so a missing login there is discovered at the one moment it
# can no longer be fixed cheaply.

# The stores a container engine keeps registry logins in. Docker and podman use
# different ones, and `docker` is a podman wrapper on some machines, so both are
# consulted rather than guessed at from the engine name.
registry_auth_files() {
  printf '%s\n' \
    "${DOCKER_CONFIG:-$HOME/.docker}/config.json" \
    "${REGISTRY_AUTH_FILE:-}" \
    "${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/containers/auth.json" \
    "$HOME/.config/containers/auth.json" \
    | grep -v '^$'
}

# True when something on this machine can authenticate to the image registry:
# credentials in the environment, or an engine that is already logged in.
#
# Mirrors the precedence in scripts/image.sh's registry_login, so the preflight
# cannot say yes to a case the push would then say no to.
have_registry_credentials() {
  local host="${REGISTRY:-docker.io}" f

  if [ -n "${REGISTRY_USER:-}" ] && [ -n "${REGISTRY_PASSWORD:-}" ]; then
    return 0
  fi
  if [ "$host" = "ghcr.io" ] && [ -n "${GITHUB_TOKEN:-}" ]; then
    return 0
  fi

  while IFS= read -r f; do
    [ -f "$f" ] || continue
    grep -q "\"$host\"" "$f" && return 0
  done < <(registry_auth_files)

  return 1
}

# --- Versions --------------------------------------------------------------
#
# A released version has exactly one home: an annotated git tag vX.Y.Z. There
# is no VERSION file to forget to bump, and nothing can claim a version that
# was never tagged.

SEMVER_RE='^[0-9]+\.[0-9]+\.[0-9]+$'

is_semver() { [[ "$1" =~ $SEMVER_RE ]]; }

# Highest released version, or empty when nothing has been released yet.
# --sort=-v:refname orders numerically, so v0.10.0 beats v0.9.0.
#
# The trailing `|| true` is load-bearing. Before the first release there are no
# tags, grep matches nothing and exits 1, and under `set -o pipefail` that would
# abort the caller - silently, since grep says nothing when it finds nothing.
latest_version() {
  git -C "$REPO_ROOT" tag --list 'v*' --sort=-v:refname 2>/dev/null \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' \
    | head -n 1 \
    | sed 's/^v//' || true
}

# major | minor | patch applied to X.Y.Z.
bump_version() {
  local current="$1" part="$2" major minor patch
  IFS=. read -r major minor patch <<<"$current"
  case "$part" in
    major) major=$((major + 1)); minor=0; patch=0 ;;
    minor) minor=$((minor + 1)); patch=0 ;;
    patch) patch=$((patch + 1)) ;;
    *) die "unknown bump: $part (expected major, minor or patch)" ;;
  esac
  printf '%s.%s.%s' "$major" "$minor" "$patch"
}

# The version a development build carries: the last tag plus the commit it was
# built from, marked -dirty when the tree has uncommitted changes. Never a bare
# X.Y.Z, so a local build cannot be mistaken for a release.
describe_version() {
  git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null | sed 's/^v//' \
    || printf 'dev'
}

tag_exists() {
  git -C "$REPO_ROOT" rev-parse -q --verify "refs/tags/$1" >/dev/null 2>&1
}

# --- Git state -------------------------------------------------------------

in_git_repo() {
  git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1
}

require_git_repo() {
  in_git_repo || die "$REPO_ROOT is not a git repository
     Run: git init && git add . && git commit -m 'Initial commit'"
  git -C "$REPO_ROOT" rev-parse HEAD >/dev/null 2>&1 \
    || die "the repository has no commits yet - commit before releasing"
}

require_clean_tree() {
  [ -z "$(git -C "$REPO_ROOT" status --porcelain)" ] \
    || die "the working tree has uncommitted changes
     Commit or stash them, or pass --allow-dirty for a test run that is not published."
}

current_branch() {
  git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD
}

# --- Release artefacts -----------------------------------------------------

# wiki-mcp_1.2.0_linux_amd64.tar.gz - the layout of the release archives.
archive_basename() {
  local version="$1" goos="$2" goarch="$3"
  printf '%s_%s_%s_%s' "$BINARY_NAME" "$version" "$goos" "$goarch"
}

archive_extension() {
  case "$1" in
    windows) printf 'zip' ;;
    *)       printf 'tar.gz' ;;
  esac
}

checksums_name() {
  printf '%s_%s_checksums.txt' "$BINARY_NAME" "$1"
}

# sha256sum on Linux, shasum -a 256 on macOS. Prints "<hash>  <file>".
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$@"
  else
    die "no sha256 tool found (install coreutils or perl's shasum)"
  fi
}
