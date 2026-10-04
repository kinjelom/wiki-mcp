# wiki-mcp

Remote [MCP](https://modelcontextprotocol.io) server for **XWiki** and **DokuWiki** with per-user OAuth. Every user adds
it as a connector in Claude and signs in with **their own wiki account**; the tools search and read the wiki with that
account's permissions. One instance serves one wiki.

## Quick start

Binaries for Linux, macOS and Windows are on the [releases page](https://github.com/kinjelom/wiki-mcp/releases), the
image is `ghcr.io/kinjelom/wiki-mcp`. Or build it (Go 1.26+):

```bash
make                                  # gofmt, vet, tests + bin/wiki-mcp
cp config.example.yml config.yml      # set server.public_url, wiki.engine, wiki.url
./bin/wiki-mcp --config.file=config.yml
```

Claude needs the server on **HTTPS, reachable from the internet** (Anthropic connects from `160.79.104.0/21`), so
put a TLS reverse proxy in front of it. Then:

- **Claude** (claude.ai, Desktop, mobile): *Settings → Connectors → Add custom connector*, URL
  `https://<public_url>/mcp`, OAuth client fields left empty.
- **Claude Code**: `claude mcp add --transport http wiki https://<public_url>/mcp`, then `/mcp` to sign in.

## Tools

| Tool                                 | What it does                                                                      |
|--------------------------------------|-----------------------------------------------------------------------------------|
| `search`                             | full-text search in the engine's own syntax, below a page (`under`), newest first |
| `get_page`                           | metadata and content: source markup, or rendered `text` / `html`; old versions    |
| `list_pages`                         | children of a page or namespace, or all pages below it                            |
| `get_page_history`                   | versions with author and summary                                                  |
| `get_recent_changes`                 | latest changes, `since` a time or duration (`7d`)                                 |
| `get_backlinks`                      | pages linking to a page                                                           |
| `list_attachments`, `get_attachment` | files of a page; text returned as text, images as images                          |
| `wiki_info`                          | the wiki, its markup and the signed-in account                                    |

The descriptions are generated for the configured wiki, so the model gets its exact page ID format and search syntax.

## Engines

### xwiki

XWiki 16.4+ (tested with 17.10.13), REST API with basic auth.

- Search: Solr — plain words, or Solr syntax with XWiki fields (`title:`, `space_prefix:`, `class:`, `property.*`,
  `date:[NOW-7DAYS TO NOW]`); no snippets.
- Page IDs: `Space.Page`, nested pages `Space.Page.WebHome`, other subwikis `subwiki:Space.Page`.

### dokuwiki

DokuWiki 2024-02-06 "Kaos"+ with the `remote` and `remoteuser` options set, JSON-RPC API v14.

- Search: DokuWiki syntax (`"phrase"`, `-word`, `word*`, `@namespace`), with snippets.
- Page IDs: `namespace:page`; attachments are the media of the page's namespace.
- Users may sign in with a token from their profile instead of the password.

## More

- [How it works](DETAILS.md#how-it-works)
- [OAuth and security](DETAILS.md#oauth-and-security)
- [Configuration](DETAILS.md#configuration)
- [Deployment](DETAILS.md#deployment)
- [Metrics](DETAILS.md#metrics)
- [Adding a wiki engine](DETAILS.md#adding-a-wiki-engine)
- [Development](DETAILS.md#development)
- [Building and releasing](RELEASING.md)

## License

[MIT](LICENSE)
