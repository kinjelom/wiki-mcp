#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# wiki-mcp - cross-compile the release matrix into dist/
#
# For every platform in RELEASE_PLATFORMS (release.conf) it produces one
# archive holding the binary, the documentation and THIRD_PARTY_LICENSES.txt,
# then one checksums file covering all of them:
#
#   dist/wiki-mcp_1.2.0_linux_amd64.tar.gz
#   dist/wiki-mcp_1.2.0_darwin_arm64.tar.gz
#   dist/wiki-mcp_1.2.0_windows_amd64.zip
#   dist/wiki-mcp_1.2.0_checksums.txt
#
# These are the files scripts/release.sh uploads to the GitHub release. The
# names are part of that contract - a consumer builds the download URL from
# them - so they are computed in one place, scripts/lib.sh.
#
# Files inside an archive sit at its root, not under a directory, so a consumer
# can extract just the binary:
#
#   tar -xzf wiki-mcp_1.2.0_linux_amd64.tar.gz wiki-mcp
#
# Usage:
#   scripts/dist.sh                     # version from git describe
#   scripts/dist.sh --version 1.2.0     # the version a release will carry
#   scripts/dist.sh --platforms "linux/amd64 linux/arm64"
# ---------------------------------------------------------------------------

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

load_config

VERSION=""
PLATFORMS=""

while [ $# -gt 0 ]; do
  case "$1" in
    --version)   VERSION="${2:?--version needs a value}"; shift ;;
    --platforms) PLATFORMS="${2:?--platforms needs a value}"; shift ;;
    -h|--help)   usage "${BASH_SOURCE[0]}"; exit 0 ;;
    *)           die "unknown argument: $1" ;;
  esac
  shift
done

need_cmd go "install it from https://go.dev/dl/"
need_cmd tar
[ -n "$VERSION" ]   || VERSION="$(describe_version)"
[ -n "$PLATFORMS" ] || PLATFORMS="$RELEASE_PLATFORMS"
case " $PLATFORMS " in
  *" windows/"*) need_cmd zip "needed for the Windows archive - install it, or drop windows/* from the platforms" ;;
esac

cd "$REPO_ROOT"

step "Packaging $BINARY_NAME $VERSION"
log "platforms  : $PLATFORMS"
log "output     : $DIST_DIR"

# A stale archive from an earlier attempt must not be uploaded alongside the
# current one, so the version's own files are cleared rather than the whole
# directory - which may hold other versions someone still wants.
mkdir -p "$DIST_DIR"
rm -f "$DIST_DIR/${BINARY_NAME}_${VERSION}_"*

# Documentation shipped inside every archive: everything someone running the
# binary might want, and nothing that only concerns maintaining it - so
# RELEASING.md is deliberately absent. A file that does not exist is skipped
# rather than failing the build.
DOC_FILES=(
  README.md
  DETAILS.md
  CHANGELOG.md
  LICENSE
  config.example.yml
)

stage_docs() {
  local target="$1" f
  for f in "${DOC_FILES[@]}"; do
    [ -f "$REPO_ROOT/$f" ] && cp "$REPO_ROOT/$f" "$target/"
  done
  return 0
}

# Windows archives are zip files; everything else is tar.gz.
make_zip() {
  local archive="$1" dir="$2"
  ( cd "$dir" && zip -q -r -X "$archive" . )
}

built=0
for platform in $PLATFORMS; do
  goos="${platform%%/*}"
  goarch="${platform##*/}"
  [ "$goos" != "$platform" ] || die "malformed platform '$platform' - expected GOOS/GOARCH"

  base="$(archive_basename "$VERSION" "$goos" "$goarch")"
  ext="$(archive_extension "$goos")"
  staging="$DIST_DIR/.stage-$base"

  rm -rf "$staging"
  mkdir -p "$staging"

  "$REPO_ROOT/scripts/build.sh" \
      --version "$VERSION" --os "$goos" --arch "$goarch" --out "$staging" \
      >/dev/null

  stage_docs "$staging"
  # the licenses of what this platform's binary contains (scripts/third-party-licenses.sh)
  GOOS="$goos" GOARCH="$goarch" "$REPO_ROOT/scripts/third-party-licenses.sh" \
      --title "$BINARY_NAME $VERSION for $goos/$goarch" > "$staging/THIRD_PARTY_LICENSES.txt" \
    || die "third-party licenses for $goos/$goarch could not be collected"

  case "$ext" in
    zip)    make_zip "$DIST_DIR/$base.$ext" "$staging" ;;
    # Archived from inside the staging directory, so entries are named
    # `wiki-mcp` and not `./wiki-mcp` - that is what makes
    # `tar -xzf archive.tar.gz wiki-mcp` extract the one file a consumer wants.
    #
    # --sort=name and a fixed mtime would make this byte-reproducible on GNU
    # tar, but BSD tar on macOS has neither; the binaries inside are
    # reproducible, which is what the checksums cover.
    tar.gz) ( cd "$staging" && tar -czf "$DIST_DIR/$base.$ext" -- * ) ;;
  esac

  rm -rf "$staging"
  ok "$(basename "$DIST_DIR/$base.$ext")  ($(du -h "$DIST_DIR/$base.$ext" | cut -f1))"
  built=$((built + 1))
done

[ "$built" -gt 0 ] || die "no platforms built - is RELEASE_PLATFORMS empty?"

step "Checksums"
checksums="$(checksums_name "$VERSION")"
( cd "$DIST_DIR" && sha256_of "${BINARY_NAME}_${VERSION}_"*.tar.gz "${BINARY_NAME}_${VERSION}_"*.zip 2>/dev/null > "$checksums" ) || true
[ -s "$DIST_DIR/$checksums" ] || die "checksum file is empty: $DIST_DIR/$checksums"
cat "$DIST_DIR/$checksums"

echo
ok "$built archive(s) in $DIST_DIR"
