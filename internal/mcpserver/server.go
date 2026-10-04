// Package mcpserver builds the MCP server: the tools common to all wikis, whose descriptions carry the wiki's own
// page ID and search syntax, plus the tools of the engine if it has any. Every tool call runs as the signed-in user of the
// request.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kinjelom/wiki-mcp/internal/oauth"
	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

type Options struct {
	Engine  wiki.Engine
	Version string
	// Revoke signs a user out when the wiki stops accepting their stored credentials.
	Revoke     func(grantID string) error
	Logger     *slog.Logger
	Registerer prometheus.Registerer
}

type builder struct {
	Options
	info    wiki.Info
	calls   *prometheus.CounterVec
	latency *prometheus.HistogramVec
}

// New builds the MCP server of the wiki.
func New(opts Options) *mcp.Server {
	b := &builder{Options: opts, info: opts.Engine.Info()}
	b.calls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wiki_mcp_tool_calls_total",
		Help: "MCP tool calls by tool and result (ok, not_found, forbidden, invalid_credentials, bad_request, error).",
	}, []string{"tool", "result"})
	b.latency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "wiki_mcp_tool_duration_seconds",
		Help:    "Duration of MCP tool calls, including the requests to the wiki.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"tool"})
	if opts.Registerer != nil {
		opts.Registerer.MustRegister(b.calls, b.latency)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "wiki-mcp",
		Title:   b.info.Name,
		Version: opts.Version,
	}, &mcp.ServerOptions{Instructions: b.instructions(), Logger: opts.Logger})
	b.addCommonTools(server)

	// optional capabilities are checked on a session without credentials, sessions of one engine are alike
	sess := opts.Engine.Session(wiki.Credential{})
	if _, ok := sess.(wiki.AttachmentReader); ok {
		b.addAttachmentTools(server)
	}
	if tp, ok := opts.Engine.(wiki.ToolProvider); ok {
		tp.AddTools(server, wiki.Tools{
			Session: func(ctx context.Context, req *mcp.CallToolRequest) (wiki.Session, error) {
				s, _, err := b.session(req)
				return s, err
			},
			Error: b.toolError,
		})
	}
	return server
}

func (b *builder) instructions() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Tools for the %s wiki (%s, %s). Every call runs as the wiki user who connected this server, with "+
		"that user's permissions: pages the user may not see are not found.\n", b.info.Name, b.info.Engine, b.info.URL)
	sb.WriteString("Find pages with search (or browse with list_pages), then read them with get_page; give users the " +
		"page url when citing a page.\n")
	sb.WriteString(b.info.PageIDHelp + "\n")
	fmt.Fprintf(&sb, "Page sources use the %s markup. The tools only read the wiki.", b.info.Markup)
	return sb.String()
}

// session returns the wiki session and the user of a tool call.
func (b *builder) session(req *mcp.CallToolRequest) (wiki.Session, *oauth.User, error) {
	u := userOf(req)
	if u == nil {
		return nil, nil, errors.New("the request is not authenticated")
	}
	return b.Engine.Session(u.Credential), u, nil
}

func userOf(req *mcp.CallToolRequest) *oauth.User {
	if req == nil || req.Extra == nil {
		return nil
	}
	return oauth.UserFromTokenInfo(req.Extra.TokenInfo)
}

// toolError turns an engine error into the message the model sees.
func (b *builder) toolError(_ context.Context, req *mcp.CallToolRequest, err error) error {
	u := userOf(req)
	switch {
	case errors.Is(err, wiki.ErrInvalidCredentials):
		if u != nil && b.Revoke != nil {
			if rerr := b.Revoke(u.GrantID); rerr != nil {
				b.Logger.Error("revoking a session", "err", rerr)
			}
		}
		return errors.New("the wiki no longer accepts the credentials of this connection (changed password or " +
			"disabled account) and the connection was signed out: ask the user to reconnect the connector and sign in again")
	case errors.Is(err, wiki.ErrForbidden):
		who := "the signed-in user"
		if u != nil {
			who = u.Identity.ID
		}
		return fmt.Errorf("access denied: the wiki account %s has no permission for this: %w", who, err)
	}
	return err
}

func resultLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, wiki.ErrNotFound):
		return "not_found"
	case errors.Is(err, wiki.ErrForbidden):
		return "forbidden"
	case errors.Is(err, wiki.ErrInvalidCredentials):
		return "invalid_credentials"
	case errors.Is(err, wiki.ErrBadRequest):
		return "bad_request"
	}
	return "error"
}

// handler is a tool implementation working on the session of the calling user.
type handler[In any] func(ctx context.Context, s wiki.Session, u *oauth.User, in In) (*mcp.CallToolResult, error)

// addTool registers a tool: it resolves the user's session, translates engine errors, logs and measures the call.
func addTool[In any](b *builder, server *mcp.Server, t *mcp.Tool, h handler[In]) {
	mcp.AddTool(server, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		start := time.Now()
		s, u, err := b.session(req)
		var res *mcp.CallToolResult
		if err == nil {
			res, err = h(ctx, s, u, in)
		}
		b.calls.WithLabelValues(t.Name, resultLabel(err)).Inc()
		b.latency.WithLabelValues(t.Name).Observe(time.Since(start).Seconds())
		user := ""
		if u != nil {
			user = u.Identity.ID
		}
		if err != nil {
			b.Logger.Info("tool call failed", "tool", t.Name, "user", user, "err", err)
			return nil, nil, b.toolError(ctx, req, err)
		}
		b.Logger.Debug("tool call", "tool", t.Name, "user", user, "duration", time.Since(start))
		return res, nil, nil
	})
}

// inputSchema infers the input schema of In and lets mod add enums and limits.
func inputSchema[In any](mod func(props map[string]*jsonschema.Schema)) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	if mod != nil {
		mod(s.Properties)
	}
	return s
}

func enum(values ...string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func between(p *jsonschema.Schema, lo, hi float64) {
	p.Minimum, p.Maximum = &lo, &hi
}

// clamp returns v limited to [1, hi], def when v is not set.
func clamp(v, def, hi int) int {
	switch {
	case v <= 0:
		return def
	case v > hi:
		return hi
	}
	return v
}

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: wiki.Ptr(false)}

func (b *builder) addCommonTools(server *mcp.Server) {
	addTool(b, server, &mcp.Tool{
		Name:  "wiki_info",
		Title: "Wiki information",
		Description: fmt.Sprintf("Describes the %s wiki and the wiki account this connection signed in with: name, "+
			"engine, URL, markup and page ID format.", b.info.Name),
		Annotations: readOnly,
	}, b.wikiInfo)

	addTool(b, server, &mcp.Tool{
		Name:  "search",
		Title: "Search the wiki",
		Description: fmt.Sprintf("Full-text search of the %s wiki (%s). Returns page IDs, titles, URLs and, where "+
			"the wiki provides them, snippets; read a page with get_page. %s", b.info.Name, b.info.Engine, b.info.SearchHelp),
		Annotations: readOnly,
		InputSchema: inputSchema[searchInput](func(p map[string]*jsonschema.Schema) {
			p["sort"].Enum = enum(string(wiki.SortRelevance), string(wiki.SortNewest))
			between(p["limit"], 1, 50)
		}),
	}, b.search)

	addTool(b, server, &mcp.Tool{
		Name:  "get_page",
		Title: "Read a page",
		Description: fmt.Sprintf("Reads a page of the %s wiki: metadata (title, URL, version, last change) and the "+
			"content. format=source (default) is the %s markup as stored; text is the page rendered by the wiki "+
			"(macros and includes expanded) as plain text; html is the rendered HTML. Long content comes in parts: "+
			"call again with offset=next_offset. %s", b.info.Name, b.info.Markup, b.info.PageIDHelp),
		Annotations: readOnly,
		InputSchema: inputSchema[getPageInput](func(p map[string]*jsonschema.Schema) {
			p["format"].Enum = enum(string(wiki.FormatSource), string(wiki.FormatText), string(wiki.FormatHTML))
			between(p["max_chars"], 1000, maxPageChars)
		}),
	}, b.getPage)

	addTool(b, server, &mcp.Tool{
		Name:  "list_pages",
		Title: "List pages",
		Description: fmt.Sprintf("Browses the page tree of the %s wiki: the direct children of a page or namespace "+
			"(parent empty for the top level), or with recursive=true all pages below it.", b.info.Name),
		Annotations: readOnly,
		InputSchema: inputSchema[listPagesInput](func(p map[string]*jsonschema.Schema) { between(p["limit"], 1, 500) }),
	}, b.listPages)

	addTool(b, server, &mcp.Tool{
		Name:        "get_page_history",
		Title:       "Page history",
		Description: "Lists the versions of a page, newest first: version, date, author and edit summary. Read an old version with get_page and version.",
		Annotations: readOnly,
		InputSchema: inputSchema[historyInput](func(p map[string]*jsonschema.Schema) { between(p["limit"], 1, 100) }),
	}, b.history)

	addTool(b, server, &mcp.Tool{
		Name:        "get_recent_changes",
		Title:       "Recent changes",
		Description: fmt.Sprintf("Lists the latest page changes in the %s wiki, newest first.", b.info.Name),
		Annotations: readOnly,
		InputSchema: inputSchema[changesInput](func(p map[string]*jsonschema.Schema) { between(p["limit"], 1, 100) }),
	}, b.recentChanges)

	addTool(b, server, &mcp.Tool{
		Name:        "get_backlinks",
		Title:       "Backlinks",
		Description: "Lists the pages that link to a page.",
		Annotations: readOnly,
		InputSchema: inputSchema[backlinksInput](func(p map[string]*jsonschema.Schema) { between(p["limit"], 1, 500) }),
	}, b.backlinks)
}

type wikiInfoInput struct{}

func (b *builder) wikiInfo(_ context.Context, _ wiki.Session, u *oauth.User, _ wikiInfoInput) (*mcp.CallToolResult, error) {
	return wiki.JSONResult(map[string]any{
		"name":      b.info.Name,
		"engine":    b.info.Engine,
		"url":       b.info.URL,
		"markup":    b.info.Markup,
		"page_ids":  b.info.PageIDHelp,
		"signed_in": u.Identity,
	})
}

type searchInput struct {
	Query  string `json:"query" jsonschema:"search query in the syntax given in the tool description"`
	Under  string `json:"under,omitempty" jsonschema:"only pages below this page or namespace ID"`
	Sort   string `json:"sort,omitempty" jsonschema:"relevance (default) or newest (last modified first)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"number of hits, default 10"`
	Offset int    `json:"offset,omitempty" jsonschema:"hits to skip, for the next page of results"`
}

func (b *builder) search(ctx context.Context, s wiki.Session, _ *oauth.User, in searchInput) (*mcp.CallToolResult, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, fmt.Errorf("%w: query is empty", wiki.ErrBadRequest)
	}
	sort := wiki.Sort(in.Sort)
	if sort == "" {
		sort = wiki.SortRelevance
	}
	res, err := s.Search(ctx, wiki.SearchRequest{
		Query: in.Query, Under: in.Under, Sort: sort, Limit: clamp(in.Limit, 10, 50), Offset: max(in.Offset, 0),
	})
	if err != nil {
		return nil, err
	}
	return wiki.JSONResult(res)
}

const (
	defaultPageChars = 40000
	maxPageChars     = 200000
)

type getPageInput struct {
	ID       string `json:"id" jsonschema:"page ID"`
	Format   string `json:"format,omitempty" jsonschema:"source (default), text or html"`
	Version  string `json:"version,omitempty" jsonschema:"an older version from get_page_history, empty for the current one"`
	Offset   int    `json:"offset,omitempty" jsonschema:"character offset in the content, for the next part of a long page"`
	MaxChars int    `json:"max_chars,omitempty" jsonschema:"maximum characters of content to return, default 40000"`
}

func (b *builder) getPage(ctx context.Context, s wiki.Session, _ *oauth.User, in getPageInput) (*mcp.CallToolResult, error) {
	format := wiki.Format(in.Format)
	if format == "" {
		format = wiki.FormatSource
	}
	page, err := s.GetPage(ctx, wiki.PageRequest{ID: in.ID, Version: in.Version, Format: format})
	if err != nil {
		return nil, err
	}
	chunk, meta := window(page.Content, max(in.Offset, 0), clamp(in.MaxChars, defaultPageChars, maxPageChars))
	meta["page"] = page
	head, err := wiki.JSONText(meta)
	if err != nil {
		return nil, err
	}
	if chunk == "" {
		chunk = "(no content)"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: head}, &mcp.TextContent{Text: chunk}}}, nil
}

// window cuts maxChars characters (runes) of content from offset and describes the cut.
func window(content string, offset, maxChars int) (string, map[string]any) {
	total := utf8.RuneCountInString(content)
	meta := map[string]any{"content_chars": total}
	if offset == 0 && total <= maxChars {
		return content, meta
	}
	runes := []rune(content)
	start := min(offset, total)
	end := min(start+maxChars, total)
	meta["offset"] = start
	meta["returned_chars"] = end - start
	if end < total {
		meta["next_offset"] = end
	}
	return string(runes[start:end]), meta
}

type listPagesInput struct {
	Parent    string `json:"parent,omitempty" jsonschema:"page or namespace ID, empty for the top level"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"all pages below parent instead of its direct children"`
	Limit     int    `json:"limit,omitempty" jsonschema:"number of items, default 100"`
	Offset    int    `json:"offset,omitempty" jsonschema:"items to skip, for the next page of results"`
}

func (b *builder) listPages(ctx context.Context, s wiki.Session, _ *oauth.User, in listPagesInput) (*mcp.CallToolResult, error) {
	res, err := s.ListPages(ctx, wiki.ListRequest{
		Parent: in.Parent, Recursive: in.Recursive, Limit: clamp(in.Limit, 100, 500), Offset: max(in.Offset, 0),
	})
	if err != nil {
		return nil, err
	}
	return wiki.JSONResult(res)
}

type historyInput struct {
	ID     string `json:"id" jsonschema:"page ID"`
	Limit  int    `json:"limit,omitempty" jsonschema:"number of versions, default 20"`
	Offset int    `json:"offset,omitempty" jsonschema:"versions to skip, for older versions"`
}

func (b *builder) history(ctx context.Context, s wiki.Session, _ *oauth.User, in historyInput) (*mcp.CallToolResult, error) {
	res, err := s.History(ctx, wiki.HistoryRequest{ID: in.ID, Limit: clamp(in.Limit, 20, 100), Offset: max(in.Offset, 0)})
	if err != nil {
		return nil, err
	}
	return wiki.JSONResult(res)
}

type changesInput struct {
	Since  string `json:"since,omitempty" jsonschema:"only changes after this time: RFC 3339 (2026-01-31T00:00:00Z), a date (2026-01-31) or a duration back from now (24h, 7d)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"number of changes, default 20"`
	Offset int    `json:"offset,omitempty" jsonschema:"changes to skip, for older changes"`
}

func (b *builder) recentChanges(ctx context.Context, s wiki.Session, _ *oauth.User, in changesInput) (*mcp.CallToolResult, error) {
	since, err := parseSince(in.Since, time.Now())
	if err != nil {
		return nil, err
	}
	res, err := s.RecentChanges(ctx, wiki.ChangesRequest{Since: since, Limit: clamp(in.Limit, 20, 100), Offset: max(in.Offset, 0)})
	if err != nil {
		return nil, err
	}
	return wiki.JSONResult(res)
}

// parseSince accepts an RFC 3339 time, a date or a duration back from now with the units of Go plus "d" for days.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err == nil && n >= 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("%w: since %q is not a time, date or duration", wiki.ErrBadRequest, s)
}

type backlinksInput struct {
	ID    string `json:"id" jsonschema:"page ID"`
	Limit int    `json:"limit,omitempty" jsonschema:"number of pages, default 100"`
}

func (b *builder) backlinks(ctx context.Context, s wiki.Session, _ *oauth.User, in backlinksInput) (*mcp.CallToolResult, error) {
	pages, err := s.Backlinks(ctx, in.ID, clamp(in.Limit, 100, 500))
	if err != nil {
		return nil, err
	}
	return wiki.JSONResult(map[string]any{"id": in.ID, "backlinks": pages})
}
