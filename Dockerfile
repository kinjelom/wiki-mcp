# ---------------------------------------------------------------------------
# wiki-mcp - the server binary in a distroless image
#
# Two stages: build a static binary, then copy it into an image that holds
# nothing else - no shell, no package manager. Built by scripts/image.sh, which
# passes the version and the base images from release.conf:
#
#     docker run -p 8090:8090 -v ./config.yml:/etc/wiki-mcp/config.yml:ro \
#       -v wiki-mcp:/var/lib/wiki-mcp ghcr.io/kinjelom/wiki-mcp
# ---------------------------------------------------------------------------

ARG GO_IMAGE=docker.io/library/golang:1.26-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build

ARG VERSION=dev
ARG REVISION=unknown
ARG BRANCH=unknown
ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/prometheus/common/version.Version=${VERSION} \
        -X github.com/prometheus/common/version.Revision=${REVISION} \
        -X github.com/prometheus/common/version.Branch=${BRANCH} \
        -X github.com/prometheus/common/version.BuildDate=$(date -u +%Y%m%d-%H:%M:%S)" \
      -o /out/wiki-mcp ./cmd/wiki-mcp
# the licenses of the code compiled into the binary, which travel with it (scripts/third-party-licenses.sh)
RUN sh scripts/third-party-licenses.sh --title "wiki-mcp ${VERSION} for $(go env GOOS)/$(go env GOARCH)" \
      > /out/THIRD_PARTY_LICENSES.txt

FROM ${RUNTIME_IMAGE}

ARG VERSION=dev
ARG REVISION=unknown
ARG SOURCE_URL=https://github.com/kinjelom/wiki-mcp

# org.opencontainers.image.source links the package on ghcr.io to the repository
LABEL org.opencontainers.image.title="wiki-mcp" \
      org.opencontainers.image.description="MCP server for XWiki and DokuWiki with per-user OAuth" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.source="${SOURCE_URL}" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/wiki-mcp /bin/wiki-mcp
COPY --from=build /out/THIRD_PARTY_LICENSES.txt /usr/share/doc/wiki-mcp/THIRD_PARTY_LICENSES.txt
COPY LICENSE /usr/share/doc/wiki-mcp/LICENSE
# sessions (bbolt) and the generated encryption key; mount a persistent volume here
WORKDIR /var/lib/wiki-mcp
EXPOSE 8090 9407
USER nonroot
ENTRYPOINT ["/bin/wiki-mcp"]
CMD ["--config.file=/etc/wiki-mcp/config.yml"]
