#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# wiki-mcp - build the binary for this machine
#
# Produces bin/wiki-mcp for the host platform (or --os/--arch). Use this while
# developing; scripts/dist.sh is what builds the release matrix.
#
# The version is baked in with -ldflags into github.com/prometheus/common/version,
# so `wiki-mcp --version` and the wiki_mcp_build_info metric report what it was
# built from. A build that is not exactly on a tag says so - 1.2.0-3-gabc or
# 1.2.0-3-gabc-dirty - and can never be mistaken for a release.
#
# Usage:
#   scripts/build.sh                    # host platform, version from git
#   scripts/build.sh --version 1.2.0    # override the version label
#   scripts/build.sh --os linux --arch arm64
# ---------------------------------------------------------------------------

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

load_config

VERSION=""
TARGET_OS=""
TARGET_ARCH=""
OUT_DIR="$BIN_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?--version needs a value}"; shift ;;
    --os)      TARGET_OS="${2:?--os needs a value}"; shift ;;
    --arch)    TARGET_ARCH="${2:?--arch needs a value}"; shift ;;
    --out)     OUT_DIR="${2:?--out needs a value}"; shift ;;
    -h|--help) usage "${BASH_SOURCE[0]}"; exit 0 ;;
    *)         die "unknown argument: $1" ;;
  esac
  shift
done

need_cmd go "install it from https://go.dev/dl/"
[ -n "$VERSION" ] || VERSION="$(describe_version)"

cd "$REPO_ROOT"
mkdir -p "$OUT_DIR"

suffix=""
[ "$TARGET_OS" = "windows" ] && suffix=".exe"

step "Building $BINARY_NAME $VERSION"
[ -n "$TARGET_OS$TARGET_ARCH" ] && log "target     : ${TARGET_OS:-$(go env GOOS)}/${TARGET_ARCH:-$(go env GOARCH)}"
log "output     : $OUT_DIR"

# CGO off is what keeps the binary static: it runs on distroless, on scratch,
# and on any Linux without matching libc versions. It is also why every
# platform in the release matrix cross-compiles without a toolchain.
export CGO_ENABLED=0
[ -n "$TARGET_OS" ]   && export GOOS="$TARGET_OS"
[ -n "$TARGET_ARCH" ] && export GOARCH="$TARGET_ARCH"

revision="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
branch="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
version_pkg=github.com/prometheus/common/version
ldflags="-s -w -X $version_pkg.Version=$VERSION -X $version_pkg.Revision=$revision -X $version_pkg.Branch=$branch"
ldflags="$ldflags -X $version_pkg.BuildDate=$(date -u +%Y%m%d-%H:%M:%S)"

# -trimpath removes the build machine's paths from the binary.
go build -trimpath -ldflags "$ldflags" -o "$OUT_DIR/$BINARY_NAME$suffix" "./cmd/$BINARY_NAME"
ok "$OUT_DIR/$BINARY_NAME$suffix"

# A binary built for another platform cannot be asked what it is.
if [ -z "$TARGET_OS" ] && [ -z "$TARGET_ARCH" ]; then
  echo
  "$OUT_DIR/$BINARY_NAME" --version
fi
