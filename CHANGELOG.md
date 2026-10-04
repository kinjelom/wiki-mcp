# Changelog

Every notable change, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

`scripts/release.sh` reads this file: it renames `[Unreleased]` to the version
being released, dates it, opens a fresh `[Unreleased]`, and uses the section it
just closed as the release notes and the annotated tag's message. So what you
write here while working is what the release says - add the entry with the
change, not at release time.

## [Unreleased]

## [0.1.0] - 2026-10-04

First public release - an early version. While wiki-mcp is at 0.x, the
configuration, tool names and tool parameters may still change in a minor
release; such changes will be listed here.

### Added

- Remote MCP server (Streamable HTTP at `/mcp`) giving Claude read-only access
  to one XWiki or DokuWiki instance. Every user signs in with their own wiki
  account, so the wiki's permissions, history and audit apply.
- Tools: `search`, `get_page`, `list_pages`, `get_page_history`,
  `get_recent_changes`, `get_backlinks`, `list_attachments`, `get_attachment`
  and `wiki_info`. Their descriptions are generated for the configured wiki,
  with its page ID format and search syntax.
- XWiki engine: XWiki 16.4+ over the REST API, Solr search, nested pages and
  subwikis.
- DokuWiki engine: DokuWiki 2024-02-06 "Kaos"+ over the JSON-RPC API v14,
  search with snippets, namespace media as attachments, sign-in with a profile
  token instead of the password.
- Built-in OAuth 2.1 authorization server: authorization code with PKCE S256,
  client ID metadata documents and dynamic client registration (RFC 7591),
  token revocation (RFC 7009), refresh token rotation with replay detection,
  and a rate-limited sign-in page.
- Wiki credentials stored encrypted (AES-256-GCM) per session, and checked
  against the wiki again on every token refresh.
- YAML configuration with environment overrides and `--config.check`; optional
  TLS through the Prometheus exporter-toolkit web configuration.
- Prometheus metrics for tool calls and OAuth on a separate listener, and a
  `/-/healthy` endpoint.
- Release archives for Linux and macOS (amd64, arm64) and Windows (amd64), with
  SHA-256 checksums and third-party licenses, and a distroless container image
  `ghcr.io/kinjelom/wiki-mcp`.
