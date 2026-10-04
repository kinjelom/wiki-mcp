#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# wiki-mcp - the test suite
#
# Three checks, cheapest first, so the slow one only runs on code that is
# already formatted and vetted:
#
#   1. gofmt   every file is formatted
#   2. go vet  the compiler's own lint
#   3. go test the unit tests, race detector optional
#
# This is what scripts/release.sh runs before it tags anything.
#
# Usage:
#   scripts/test.sh              # gofmt, vet, test
#   scripts/test.sh --race       # ... with the race detector
#   scripts/test.sh --cover      # ... with a coverage summary
# ---------------------------------------------------------------------------

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

RACE=0
COVER=0
while [ $# -gt 0 ]; do
  case "$1" in
    --race)    RACE=1 ;;
    --cover)   COVER=1 ;;
    -h|--help) usage "${BASH_SOURCE[0]}"; exit 0 ;;
    *)         die "unknown argument: $1" ;;
  esac
  shift
done

need_cmd go "install it from https://go.dev/dl/"
cd "$REPO_ROOT"

step "Formatting"
unformatted="$(gofmt -l . || true)"
if [ -n "$unformatted" ]; then
  printf '%s\n' "$unformatted" >&2
  die "the files above are not gofmt-formatted - run: gofmt -w ."
fi
ok "gofmt clean"

step "Vet"
go vet ./...
ok "go vet clean"

step "Tests"
test_args=(./...)
# The race detector needs cgo, which the release build deliberately does not
# use. It is a test-time tool only, so CGO is enabled just for this command.
[ "$RACE" -eq 1 ] && test_args=(-race "${test_args[@]}")
[ "$COVER" -eq 1 ] && test_args=(-coverprofile=coverage.out "${test_args[@]}")

if [ "$RACE" -eq 1 ]; then
  CGO_ENABLED=1 go test "${test_args[@]}"
else
  go test "${test_args[@]}"
fi

if [ "$COVER" -eq 1 ]; then
  go tool cover -func=coverage.out | tail -n 1
fi

echo
ok "all checks passed"
