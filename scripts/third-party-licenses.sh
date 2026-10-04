#!/bin/sh
# ---------------------------------------------------------------------------
# wiki-mcp - licenses of the code compiled into the binary
#
# Writes to stdout the license texts of the Go standard library and runtime and
# of every module `go list -deps` reports for ./cmd/wiki-mcp on the target
# platform - GOOS/GOARCH from the environment, as for `go build`. That is what
# the binary contains, not what go.mod mentions: test dependencies are left out,
# and some modules are linked on some platforms only. Their licenses (MIT, BSD,
# Apache 2.0) require the license text to travel with the binary, Apache 2.0
# also the NOTICE file, so every LICENSE, COPYING and NOTICE file at a module's
# root is copied. A module without one is an error: shipping it would need its
# license checked by hand first.
#
# scripts/dist.sh puts the result into every release archive and the
# Dockerfile into the image, both as THIRD_PARTY_LICENSES.txt. The modules must
# be in the module cache (go mod download, or a build before).
#
# POSIX sh rather than bash like the other scripts: it also runs in the alpine
# build stage of the Dockerfile, which has no bash.
#
# Usage:
#   scripts/third-party-licenses.sh > THIRD_PARTY_LICENSES.txt
#   GOOS=windows GOARCH=amd64 scripts/third-party-licenses.sh --title "wiki-mcp 1.2.0 for windows/amd64"
# ---------------------------------------------------------------------------

set -eu

cd "$(dirname "$0")/.."

PACKAGE=./cmd/wiki-mcp
TITLE=""

fail() {
  printf 'ERROR %s\n' "$*" >&2
  exit 1
}

while [ $# -gt 0 ]; do
  case "$1" in
    --title)   [ $# -ge 2 ] || fail "--title needs a value"; TITLE="$2"; shift ;;
    --package) [ $# -ge 2 ] || fail "--package needs a value"; PACKAGE="$2"; shift ;;
    -h|--help) awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); if ($0 !~ /^-+$/) print; next } { exit }' "$0"; exit 0 ;;
    *)         fail "unknown argument: $1" ;;
  esac
  shift
done

[ -n "$TITLE" ] || TITLE="wiki-mcp for $(go env GOOS)/$(go env GOARCH)"

# Captured before it is sorted: a failing `go list` in a pipe would be hidden by
# sort's exit status, and POSIX sh has no pipefail.
deps="$(CGO_ENABLED=0 go list -deps \
  -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{with .Replace}}{{.Dir}}{{else}}{{.Dir}}{{end}}{{end}}{{end}}' \
  "$PACKAGE")"

rule='================================================================================'

printf 'Third-party software in %s.\n\n' "$TITLE"
printf 'wiki-mcp itself is licensed under the terms in LICENSE. The binary also contains\n'
printf 'the software below, distributed under the licenses that follow it.\n'

printf '\n%s\nGo standard library and runtime %s\n%s\n\n' "$rule" "$(go env GOVERSION)" "$rule"
cat "$(go env GOROOT)/LICENSE"

# The loop runs in a subshell (it reads a pipe), so `fail` ends the loop only;
# set -e then ends the script, as the pipe's status is the loop's.
printf '%s\n' "$deps" | sort -u | while read -r path version dir; do
  [ -n "$path" ] || continue
  [ -n "$dir" ] || fail "module $path $version is not in the module cache - run: go mod download"
  files="$(find "$dir" -maxdepth 1 -type f \( -iname 'licen[cs]e*' -o -iname 'copying*' -o -iname 'notice*' \) | sort)"
  [ -n "$files" ] || fail "module $path $version has no LICENSE, COPYING or NOTICE file in $dir - check its license"
  printf '\n%s\n%s %s\n%s\n' "$rule" "$path" "$version" "$rule"
  printf '%s\n' "$files" | while IFS= read -r f; do
    printf '\n--- %s ---\n\n' "${f##*/}"
    cat "$f"
  done
done
