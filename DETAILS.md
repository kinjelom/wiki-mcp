# wiki-mcp details

How wiki-mcp works, how it keeps users' credentials safe, how to configure, deploy and monitor it, and how to extend
it. The overview and the quick start are in the [README](README.md).

- [How it works](#how-it-works)
- [OAuth and security](#oauth-and-security)
- [Configuration](#configuration)
- [Deployment](#deployment)
- [Metrics](#metrics)
- [Adding a wiki engine](#adding-a-wiki-engine)
- [Development](#development)

## How it works

```
Claude ──HTTPS──▶ reverse proxy (TLS) ──▶ wiki-mcp :8090 ── REST / JSON-RPC as the user ──▶ XWiki / DokuWiki
  │                                          │  /mcp            MCP Streamable HTTP, bearer token
  │  OAuth 2.1 + PKCE                        │  /authorize      sign-in page: user name + password, checked by the wiki
  └─ client ID metadata document (CIMD) ─────┤  /token          tokens, refresh token rotation
     or dynamic registration                 │  /register       RFC 7591
                                             │  /revoke         RFC 7009
                                             └  /.well-known/oauth-protected-resource, oauth-authorization-server
```

MCP servers for wikis usually call the wiki with one technical account: everyone using the connector sees what that
account sees and edits as that account. `wiki-mcp` is its own OAuth authorization server whose sign-in page checks
the user name and password against the wiki. The server keeps the credential (encrypted) for the session and calls
the wiki as that user, so the wiki's own access rights, history and audit apply. Two wikis are two instances (two
connectors), each signed in with that wiki's account.

## OAuth and security

- **Client registration**: Claude identifies itself with its client ID metadata document
  (`https://claude.ai/oauth/mcp-oauth-client-metadata`, MCP 2025-11-25). Documents are fetched only from
  `oauth.client_metadata_hosts` (default `claude.ai`). Other clients use dynamic client registration.
- **Redirect URIs** are limited to `oauth.allowed_redirect_uris`: by default the Claude apps and Claude Code's
  loopback redirects on any port.
- **Authorization code + PKCE S256** only; codes live a minute and are single-use; `iss` is returned (RFC 9207);
  `resource` must be this server's MCP URL (RFC 8707).
- **Sign-in page**: stateless until submitted (the request travels sealed in the form), cannot be framed, sends no
  referrer, shows the redirect host. 10 failed sign-ins per user name or client address in 15 minutes block further
  attempts (`oauth.trust_forwarded_for` behind a proxy).
- **Tokens** are random and stored as SHA-256 hashes (bbolt in `oauth.data_dir`). Access tokens live 1h, refresh
  tokens 30 days and rotate on every use; a replayed refresh token revokes the session. Sessions end after 90 days.
- **Credentials** are encrypted with AES-256-GCM, bound to their session, and checked against the wiki again on
  every refresh. When the wiki rejects them (changed password, disabled account), the session is revoked and Claude
  asks the user to sign in again.
- **Encryption key**: `WIKI_MCP_ENCRYPTION_KEY` or a key file created on the first start. Changing it signs everybody
  out. Keep the key and the store on different backups.

Wikis that authenticate only through SSO (no password the API accepts) are not supported yet.

## Configuration

YAML file (`--config.file`), commented example: [`config.example.yml`](config.example.yml). Environment overrides:

| Variable                  | Meaning                                                                             |
|---------------------------|-------------------------------------------------------------------------------------|
| `WIKI_MCP_PUBLIC_URL`     | `server.public_url`, e.g. `https://wiki-mcp.example.com`                            |
| `WIKI_MCP_WIKI_URL`       | `wiki.url`                                                                          |
| `WIKI_MCP_WIKI_ENGINE`    | `wiki.engine`                                                                       |
| `WIKI_MCP_ENCRYPTION_KEY` | key of the stored credentials (64 hex, base64 of 32 bytes or 32+ random characters) |
| `WIKI_MCP_CONFIG_FILE`    | path of the configuration file (= `--config.file`)                                  |

Flags: `--web.listen-address` (`:8090`), `--web.config.file`
(TLS, [exporter-toolkit](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)),
`--web.metrics-address` (`:9407`, empty disables), `--log.level`, `--log.format`, `--config.check`.

## Deployment

The reverse proxy must route at least `/mcp`, `/authorize`, `/token`, `/register`, `/revoke` and
`/.well-known/oauth-*` to the server. The sign-in page opens in the user's browser, so it must be reachable from the
users too. Team and Enterprise plans: an Owner adds the connector under *Organization settings → Connectors*, members
then connect and sign in individually.

Container image (distroless, nonroot, data in `/var/lib/wiki-mcp`):

```bash
docker run -p 8090:8090 -v ./config.yml:/etc/wiki-mcp/config.yml:ro -v wiki-mcp:/var/lib/wiki-mcp \
  ghcr.io/kinjelom/wiki-mcp:latest
```

The simplest setup is a second container in the wiki's pod: it calls the wiki at `http://127.0.0.1:8080`. In a
Kubernetes-style pod (also podman `kube play`, e.g. the
BOSH [pods release](https://github.com/kinjelom/pods-boshrelease)):

```yaml
initContainers:
  # the image runs as nonroot (65532), a hostPath directory is created as root
  - name: wiki-mcp-data
    image: docker.io/library/busybox:1.37
    command: [ chown, "65532:65532", /data ]
    volumeMounts: [ { name: wiki-mcp-data, mountPath: /data } ]
containers:
  - name: wiki-mcp
    image: <registry>/wiki-mcp:<version>
    args: [ --config.file=/etc/wiki-mcp/config.yml ]
    env:
      - name: WIKI_MCP_ENCRYPTION_KEY       # e.g. a generated CredHub password of 64 characters
        valueFrom: { secretKeyRef: { name: wiki-mcp, key: encryption_key } }
    volumeMounts:
      - { name: wiki-mcp-config, mountPath: /etc/wiki-mcp, readOnly: true }   # ConfigMap with config.yml
      - { name: wiki-mcp-data, mountPath: /var/lib/wiki-mcp }                 # sessions, keep it persistent
```

The image has no shell or curl, so no exec probes; monitor it through `/metrics` or `/-/healthy`.

## Metrics

On `--web.metrics-address`: `wiki_mcp_tool_calls_total{tool,result}`, `wiki_mcp_tool_duration_seconds{tool}`,
`wiki_mcp_oauth_logins_total{result}`, `wiki_mcp_oauth_tokens_issued_total{grant_type}`,
`wiki_mcp_oauth_token_errors_total{grant_type,error}`, `wiki_mcp_oauth_refresh_token_reuse_total{action}`,
`wiki_mcp_oauth_sessions`, `wiki_mcp_oauth_registered_clients`, `wiki_mcp_build_info`, `go_*`, `process_*`.

## Adding a wiki engine

1. A package under `internal/wiki/<engine>` with an `Engine` (`Info`, `Login`, `Session`) and a `Session`
   implementing `wiki.Session`. Map the wiki's errors to `wiki.ErrNotFound`, `ErrForbidden`,
   `ErrInvalidCredentials` (the wiki rejects the stored credential), `ErrBadRequest`.
2. Describe the engine in `Info`: page ID format and search syntax. They become the tool descriptions, so write them
   for the model.
3. Optional capabilities: `wiki.AttachmentReader` on the session; engine-specific tools with `wiki.ToolProvider`
   (named `<engine>_<tool>`).
4. Register it in `newEngine` (`cmd/wiki-mcp/main.go`) and `config.Engines`.

## Development

```bash
make test        # gofmt, vet, unit tests and an end-to-end OAuth + MCP test against a fake wiki
# against a running XWiki (creates pages under WikiMcpTest)
XWIKI_URL=http://127.0.0.1:8080 XWIKI_USER=Admin XWIKI_PASSWORD=... go test -tags live -run Live ./internal/wiki/xwiki/
```

- `internal/wiki` — the engine-independent model; `internal/wiki/xwiki`, `internal/wiki/dokuwiki` — engines
- `internal/oauth` — the authorization server and token store
- `internal/mcpserver` — the MCP tools
- `cmd/wiki-mcp` — wiring, HTTP servers

Building and releasing: [RELEASING.md](RELEASING.md).
