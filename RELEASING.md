# Building and releasing

Everything goes through the scripts in [`scripts/`](scripts); `make` targets are shortcuts to them. Building needs
only Go; a release also needs `gh` and a container engine.

## Building

```bash
scripts/test.sh                          # gofmt, go vet, go test (--race, --cover)
```
```bash
scripts/build.sh                         # bin/wiki-mcp for this machine
```
```bash
scripts/build.sh --os linux --arch amd64 # another platform
```
```bash
scripts/dist.sh                          # release archives for every platform into dist/
```
```bash
scripts/image.sh                         # the container image in the local store, checked with --version
```

`make` runs the tests and the build. A build that is not a release carries the `git describe` version
(`0.1.0-3-gabc1234`, `-dirty` with uncommitted changes) - see `bin/wiki-mcp --version`. Tests against a running
XWiki are described in [DETAILS.md](DETAILS.md#development).

## The scripts

Each does one thing and can be run alone; `release.sh` is the orchestration.

| Script               | Does                                             | Run it directly when                     |
|----------------------|--------------------------------------------------|------------------------------------------|
| `scripts/test.sh`    | gofmt, `go vet`, `go test` (`--race`, `--cover`) | always, before pushing                   |
| `scripts/build.sh`   | the host binary into `bin/`                      | developing                               |
| `scripts/dist.sh`    | cross-compiled archives + checksums into `dist/` | you want the artefacts without a release |
| `scripts/image.sh`   | the container image, optionally pushed           | testing the image                        |
| `scripts/release.sh` | all of the above, plus tag, push and publish     | releasing                                |

`make test|build|dist|image` are shortcuts to them. Configuration - GitHub coordinates, registry, platforms, base
images - is in [`release.conf`](release.conf), parsed and never sourced, with the environment beating the file:

```bash
IMAGE_NAMESPACE=acme GITHUB_OWNER=acme scripts/release.sh patch
```

## Releasing

One command turns a clean checkout into a published version:

```bash
scripts/release.sh patch
```

That produces an annotated tag, archives for five platforms with SHA-256 checksums, the container image
`ghcr.io/kinjelom/wiki-mcp:<version>` (+ `:latest`), and a GitHub release with the archives attached and notes taken
from the changelog. The rest of this file is about releases.

### The version has one home

There is no `VERSION` file. **The released version is the git tag** `vX.Y.Z`, and `scripts/release.sh` computes the
next one from the highest existing tag. Every other build is named by `git describe` - `1.2.0-3-gabc1234`, or
`-dirty` - and can never be mistaken for a release. The version is baked into the binary
(`github.com/prometheus/common/version`), so `wiki-mcp --version` and the `wiki_mcp_build_info` metric report what it
was built from.

### What `release.sh` does, in order

```
1. preflight   clean tree, on DEFAULT_BRANCH, tag free, gh and registry credentials present
2. test        scripts/test.sh
3. changelog   [Unreleased] becomes [X.Y.Z] - <today>, a fresh [Unreleased] opens
4. package     scripts/dist.sh  - five archives, one checksums file
5. image       scripts/image.sh - built, and its version checked by running it
6. tag         commit the changelog, annotate vX.Y.Z with the release notes
7. publish     push branch + tag, push the image, gh release create + upload
```

**Nothing leaves the machine before step 7**, and everything that can fail has failed by then. If publishing fails
after the tag was pushed, the script says what already landed and prints the commands that finish the rest.

### The changelog is the release notes

`CHANGELOG.md` has an `## [Unreleased]` section. At release time it is renamed to the version and dated, a new empty
`[Unreleased]` opens above it, and the closed section becomes the tag message and the GitHub release body. Write the
entry in the same commit as the change. Without an `[Unreleased]` section the script falls back to
`gh release create --generate-notes`.

### Artefacts

```
dist/wiki-mcp_1.2.0_linux_amd64.tar.gz      dist/wiki-mcp_1.2.0_darwin_arm64.tar.gz
dist/wiki-mcp_1.2.0_linux_arm64.tar.gz      dist/wiki-mcp_1.2.0_windows_amd64.zip
dist/wiki-mcp_1.2.0_darwin_amd64.tar.gz     dist/wiki-mcp_1.2.0_checksums.txt
```

Each archive holds the binary, `README.md`, `DETAILS.md`, `CHANGELOG.md`, `LICENSE`, `config.example.yml` and
`THIRD_PARTY_LICENSES.txt` at its root. `scripts/third-party-licenses.sh` generates the last one for each platform
from `go list -deps`: the license texts (and the NOTICE files of the Apache 2.0 licensed modules) of the Go runtime and
of every module compiled into that binary, as their licenses require for binary distribution. A module without a
license file stops the build.

```bash
tar -xzf wiki-mcp_1.2.0_linux_amd64.tar.gz wiki-mcp
```

The binaries are static (`CGO_ENABLED=0`) and built with `-trimpath`.

### The container image

`scripts/image.sh` builds `ghcr.io/kinjelom/wiki-mcp:<version>` and `:latest` (unless `IMAGE_LATEST_TAG` is `none`),
then runs the image's own `--version`. The image carries `LICENSE` and the `THIRD_PARTY_LICENSES.txt` of its binary in
`/usr/share/doc/wiki-mcp/`. Its `org.opencontainers.image.source` label links the package on ghcr.io to
this repository. `--multi-arch` builds every platform in `IMAGE_PLATFORMS` through buildx and implies `--push`.

Pushing to ghcr.io needs a token with the **`write:packages`** scope, which `gh auth login` does not ask for:

```bash
gh auth refresh -s write:packages
export GITHUB_TOKEN=$(gh auth token)
```

The preflight refuses to start without registry credentials; `--no-image` releases the binaries alone. A new
package on ghcr.io is private: make it public once under *Package settings* if the image should be pulled without a
login.

### The first release

There are no tags yet, so the current version counts as `0.0.0`:

```bash
scripts/release.sh minor --dry-run   # check it, changes and publishes nothing
scripts/release.sh minor             # 0.0.0 -> 0.1.0
```

### Useful variations

```bash
scripts/release.sh patch                    # 1.2.0 -> 1.2.1
scripts/release.sh minor                    # 1.2.0 -> 1.3.0
scripts/release.sh major                    # 1.2.0 -> 2.0.0
scripts/release.sh 2.0.0-rc.1               # an exact version, pre-release allowed
scripts/release.sh patch --dry-run          # everything local, nothing changed or published
scripts/release.sh patch --no-image         # skip the container image
scripts/release.sh patch --no-publish       # tag locally, push nothing
scripts/release.sh patch --yes              # no confirmation prompt
```

## Requirements

| Step              | Needs                                                                    |
|-------------------|--------------------------------------------------------------------------|
| test, build, dist | Go 1.26+ (`GOTOOLCHAIN=auto` fetches it if yours is older)               |
| image             | Docker or podman; buildx for `--multi-arch`                              |
| image push        | a registry login, or `GITHUB_TOKEN` with `write:packages` for ghcr.io    |
| publish           | `git`, [`gh`](https://cli.github.com/) authenticated, an `origin` remote |
