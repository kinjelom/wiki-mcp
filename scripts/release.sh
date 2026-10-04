#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# wiki-mcp - cut a release
#
# One command takes the repository from "the code is ready" to "version X.Y.Z
# exists on GitHub, with binaries for every platform in RELEASE_PLATFORMS and a
# container image".
#
#   1. preflight   git is clean, on the release branch, the tag is free,
#                  the tools needed for the steps you asked for are installed
#   2. test        scripts/test.sh - gofmt, vet, unit tests
#   3. changelog   promote the Unreleased section to the new version
#   4. package     scripts/dist.sh - archives + SHA-256 checksums
#   5. image       scripts/image.sh - the container image
#   6. tag         commit the changelog, annotate the tag vX.Y.Z
#   7. publish     push the branch and the tag, push the image,
#                  create the GitHub release and upload the archives
#
# Nothing leaves the machine before step 7, and everything that can fail has
# failed by then - so a half-published release is not a state you can reach by
# a test failing.
#
# The version lives in git tags and nowhere else. There is no VERSION file to
# forget, and a version that was never tagged cannot be claimed.
#
# Usage:
#   scripts/release.sh patch              # 1.2.0 -> 1.2.1
#   scripts/release.sh minor              # 1.2.0 -> 1.3.0
#   scripts/release.sh major              # 1.2.0 -> 2.0.0
#   scripts/release.sh 2.0.0-rc.1         # an exact version
#   scripts/release.sh patch --dry-run    # print the plan, change nothing
#
# Flags:
#   --dry-run        do everything local, publish nothing, touch no file
#   --no-image       skip the container image
#   --no-publish     build and tag locally, push nothing
#   --allow-dirty    release from a working tree with uncommitted changes
#   --allow-branch   release from a branch other than DEFAULT_BRANCH
#   --notes-file F   use F as the release notes instead of the changelog
#   --yes            do not ask for confirmation
#
# Credentials come from the environment: GITHUB_TOKEN for gh and ghcr.io, or
# REGISTRY_USER / REGISTRY_PASSWORD for another registry.
# ---------------------------------------------------------------------------

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

load_config

BUMP=""
DRY_RUN=0
DO_IMAGE=1
DO_PUBLISH=1
ALLOW_DIRTY=0
ALLOW_BRANCH=0
ASSUME_YES=0
NOTES_FILE=""

while [ $# -gt 0 ]; do
  case "$1" in
    major|minor|patch)     BUMP="$1" ;;
    --dry-run)             DRY_RUN=1 ;;
    --no-image)            DO_IMAGE=0 ;;
    --no-publish)          DO_PUBLISH=0 ;;
    --allow-dirty)         ALLOW_DIRTY=1 ;;
    --allow-branch)        ALLOW_BRANCH=1 ;;
    --yes|-y)              ASSUME_YES=1 ;;
    --notes-file)          NOTES_FILE="${2:?--notes-file needs a path}"; shift ;;
    -h|--help)             usage "${BASH_SOURCE[0]}"; exit 0 ;;
    -*)                    die "unknown flag: $1" ;;
    *)
      [ -z "$BUMP" ] || die "version given twice: $BUMP and $1"
      BUMP="$1"
      ;;
  esac
  shift
done

[ -n "$BUMP" ] || die "say what to release: major, minor, patch, or an exact version
     scripts/release.sh --help"

# --- 1. Preflight -----------------------------------------------------------

step "Preflight"

require_git_repo
need_cmd go "install it from https://go.dev/dl/"

branch="$(current_branch)"
if [ "$branch" != "$DEFAULT_BRANCH" ] && [ "$ALLOW_BRANCH" -eq 0 ]; then
  die "on branch '$branch', but releases are cut from '$DEFAULT_BRANCH'
     Switch branches, or pass --allow-branch if this is deliberate."
fi

if [ "$ALLOW_DIRTY" -eq 0 ]; then
  require_clean_tree
else
  [ -z "$(git -C "$REPO_ROOT" status --porcelain)" ] || warn "releasing from a dirty working tree (--allow-dirty)"
fi

CURRENT="$(latest_version)"
if [ -z "$CURRENT" ]; then
  log "no release tag found - this is the first release"
  CURRENT="0.0.0"
fi

case "$BUMP" in
  major|minor|patch) VERSION="$(bump_version "$CURRENT" "$BUMP")" ;;
  *)
    VERSION="${BUMP#v}"
    # An exact version may carry a pre-release suffix; the computed ones never do.
    [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] \
      || die "'$BUMP' is neither major, minor, patch, nor a version like 1.2.3"
    ;;
esac

TAG="v$VERSION"
tag_exists "$TAG" && die "tag $TAG already exists - a released version is never re-cut
     Release a different version, or delete the tag if it was never pushed.

     If a previous run tagged and pushed but failed before finishing, do not
     re-cut it: finish the remaining steps by hand instead.
       git ls-remote --tags origin $TAG          # is the tag public already?
       gh release view $TAG --repo $GITHUB_OWNER/$GITHUB_REPO
       scripts/image.sh --version $VERSION --push"

if [ "$DO_PUBLISH" -eq 1 ] && [ "$DRY_RUN" -eq 0 ]; then
  need_cmd gh "install it from https://cli.github.com/ - or pass --no-publish and upload dist/ by hand"
  gh auth status >/dev/null 2>&1 || die "gh is not authenticated - run: gh auth login"
  git -C "$REPO_ROOT" remote get-url origin >/dev/null 2>&1 \
    || die "no 'origin' remote - add it, or pass --no-publish
     git remote add origin git@github.com:$GITHUB_OWNER/$GITHUB_REPO.git"
fi

if [ "$DO_IMAGE" -eq 1 ]; then
  resolve_docker
  # The image push is the last thing a release does, long after the branch and
  # the tag are on the remote. Checking the login here is what keeps the promise
  # at the top of this file: everything that can fail has failed before step 7.
  if [ "$DO_PUBLISH" -eq 1 ] && [ "$DRY_RUN" -eq 0 ] && ! have_registry_credentials; then
    die "no credentials for ${REGISTRY:-Docker Hub} - the image push would fail in step 7,
     with the branch and the tag already pushed. Refusing here instead.

     Fix it with one of:
       export GITHUB_TOKEN=<token with the 'write:packages' scope>
       $DOCKER login ${REGISTRY:-docker.io}
       export REGISTRY_USER=... REGISTRY_PASSWORD=...

     A token from 'gh auth login' is not enough on its own: gh does not ask for
     'write:packages'. Add it with: gh auth refresh -s write:packages

     Or release without the container image: scripts/release.sh $BUMP --no-image"
  fi
fi

ok "releasing $CURRENT -> $VERSION"

# --- The plan ---------------------------------------------------------------

CHANGELOG="$REPO_ROOT/CHANGELOG.md"
NOTES="$DIST_DIR/release-notes-$VERSION.md"

echo
log "tag        : $TAG"
log "branch     : $branch"
log "archives   : $RELEASE_PLATFORMS"
if [ "$DO_IMAGE" -eq 1 ]; then
  log "image      : $IMAGE_REPO:$VERSION${IMAGE_LATEST_TAG:+ (+ :$IMAGE_LATEST_TAG)}"
else
  log "image      : skipped (--no-image)"
fi
if [ "$DRY_RUN" -eq 1 ]; then
  log "publish    : nothing - this is a dry run"
elif [ "$DO_PUBLISH" -eq 1 ]; then
  log "publish    : $(git -C "$REPO_ROOT" remote get-url origin 2>/dev/null || echo 'origin')"
else
  log "publish    : nothing (--no-publish)"
fi

if [ "$ASSUME_YES" -eq 0 ] && [ "$DRY_RUN" -eq 0 ]; then
  echo
  printf 'Release %s? [y/N] ' "$TAG"
  read -r reply || reply=""
  case "$reply" in [yY]|[yY][eE][sS]) ;; *) die "cancelled" ;; esac
fi

# --- 2. Test ----------------------------------------------------------------

"$REPO_ROOT/scripts/test.sh"

# --- 3. Changelog -----------------------------------------------------------
#
# The Unreleased section becomes the new version's section, and a fresh empty
# Unreleased takes its place. Everything the release notes say therefore came
# from the file people edited while working, not from commit subjects.

changelog_touched=0

promote_changelog() {
  [ -f "$CHANGELOG" ] || { warn "no CHANGELOG.md - release notes will be generated from commits"; return 0; }
  grep -q '^## \[Unreleased\]' "$CHANGELOG" \
    || { warn "CHANGELOG.md has no '## [Unreleased]' section - leaving it alone"; return 0; }

  local today; today="$(date +%Y-%m-%d)"
  local tmp; tmp="$(mktemp)"
  awk -v version="$VERSION" -v today="$today" '
    /^## \[Unreleased\]/ && !done {
      print "## [Unreleased]"
      print ""
      print "## [" version "] - " today
      done = 1
      next
    }
    { print }
  ' "$CHANGELOG" > "$tmp"
  # Written back through the existing file rather than moved over it: mktemp
  # creates 0600, and `mv` would carry that mode onto a tracked, world-readable
  # file - silently, since git records only the execute bit.
  cat "$tmp" > "$CHANGELOG"
  rm -f "$tmp"
  changelog_touched=1
  ok "CHANGELOG.md: [Unreleased] -> [$VERSION] - $today"
}

# Everything between this version's heading and the next one.
extract_notes() {
  mkdir -p "$DIST_DIR"
  if [ -n "$NOTES_FILE" ]; then
    [ -f "$NOTES_FILE" ] || die "notes file not found: $NOTES_FILE"
    cp "$NOTES_FILE" "$NOTES"
    return 0
  fi
  if [ -f "$CHANGELOG" ]; then
    awk -v version="$VERSION" '
      $0 ~ "^## \\[" version "\\]" { inside = 1; next }
      inside && /^## / { exit }
      inside { print }
    ' "$CHANGELOG" | sed '/./,$!d' > "$NOTES"
  fi
  [ -s "$NOTES" ] || rm -f "$NOTES"
}

if [ "$DRY_RUN" -eq 1 ]; then
  step "Changelog"
  log "would promote [Unreleased] to [$VERSION] in CHANGELOG.md"
else
  step "Changelog"
  promote_changelog
  extract_notes
fi

# --- 4. Package -------------------------------------------------------------

"$REPO_ROOT/scripts/dist.sh" --version "$VERSION"

# --- 5. Image ---------------------------------------------------------------

if [ "$DO_IMAGE" -eq 1 ]; then
  "$REPO_ROOT/scripts/image.sh" --version "$VERSION"
fi

# --- Dry run stops here -----------------------------------------------------

if [ "$DRY_RUN" -eq 1 ]; then
  step "Dry run complete"
  log "built and verified $VERSION without changing a tracked file"
  log "nothing was committed, tagged or pushed"
  echo
  ok "run again without --dry-run to release $TAG"
  exit 0
fi

# --- 6. Tag -----------------------------------------------------------------

step "Tag $TAG"

if [ "$changelog_touched" -eq 1 ]; then
  git -C "$REPO_ROOT" add CHANGELOG.md
  git -C "$REPO_ROOT" commit -m "Release $VERSION" >/dev/null
  ok "committed CHANGELOG.md"
fi

# Annotated, not lightweight: an annotated tag carries an author, a date and a
# message, and is what `git describe` and the release tooling read.
#
# --cleanup=verbatim because the notes are Markdown. Git's default cleanup
# strips every line beginning with '#', which would silently delete the
# changelog's own headings from the tag message.
if [ -s "$NOTES" ]; then
  git -C "$REPO_ROOT" tag -a "$TAG" --cleanup=verbatim -F "$NOTES"
else
  git -C "$REPO_ROOT" tag -a "$TAG" -m "Release $VERSION"
fi
ok "tagged $TAG"

if [ "$DO_PUBLISH" -eq 0 ]; then
  step "Local release complete"
  log "archives   : $DIST_DIR"
  log "tag        : $TAG (not pushed)"
  echo
  ok "push it yourself with: git push origin $branch --follow-tags"
  exit 0
fi

# --- 7. Publish -------------------------------------------------------------

step "Publish"

# From here a failure leaves something half-published. Unwinding is rarely what
# you want once the tag is on the remote - somebody may already have fetched it -
# so the recovery says what did land and how to finish the rest by hand.
PUBLISHED="nothing yet"

recover() {
  warn "publishing failed - $TAG is half-published"
  warn "already on the remote: $PUBLISHED"
  warn "re-running release.sh will not help: it stops at 'tag $TAG already exists'"
  warn "fix the cause, then finish the remaining steps by hand:"
  if [ "$DO_IMAGE" -eq 1 ]; then
    warn "  scripts/image.sh --version $VERSION --push"
  fi
  if [ -s "$NOTES" ]; then
    warn "  gh release create $TAG --repo $GITHUB_OWNER/$GITHUB_REPO --title $TAG --notes-file $NOTES $DIST_DIR/${BINARY_NAME}_${VERSION}_*"
  else
    warn "  gh release create $TAG --repo $GITHUB_OWNER/$GITHUB_REPO --title $TAG --generate-notes $DIST_DIR/${BINARY_NAME}_${VERSION}_*"
  fi
  warn "or unwind it: git push --delete origin $TAG && git tag -d $TAG"
  exit 1
}
trap recover ERR

git -C "$REPO_ROOT" push origin "$branch"
PUBLISHED="branch $branch"
git -C "$REPO_ROOT" push origin "$TAG"
PUBLISHED="branch $branch, tag $TAG"
ok "pushed $branch and $TAG"

if [ "$DO_IMAGE" -eq 1 ]; then
  "$REPO_ROOT/scripts/image.sh" --version "$VERSION" --push
  PUBLISHED="$PUBLISHED, image $IMAGE_REPO:$VERSION"
fi

notes_args=(--generate-notes)
if [ -s "$NOTES" ]; then
  notes_args=(--notes-file "$NOTES")
fi

gh release create "$TAG" \
  --repo "$GITHUB_OWNER/$GITHUB_REPO" \
  --title "$TAG" \
  "${notes_args[@]}" \
  "$DIST_DIR/${BINARY_NAME}_${VERSION}_"*

trap - ERR

echo
ok "released $TAG"
log "https://github.com/$GITHUB_OWNER/$GITHUB_REPO/releases/tag/$TAG"
if [ "$DO_IMAGE" -eq 1 ]; then
  log "$IMAGE_REPO:$VERSION"
fi
