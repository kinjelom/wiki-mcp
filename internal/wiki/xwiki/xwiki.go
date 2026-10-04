// Package xwiki is the XWiki engine: it reads and writes pages through the XWiki REST API (/rest) as the signed-in
// user, searches with Solr (/rest/wikis/{wiki}/query?type=solr) and renders pages with the "get" action.
// Requires XWiki 16.4 or newer (the XWiki-User response header, the nested page children resources).
package xwiki

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

// maxRenderedBytes limits a rendered page read from XWiki.
const maxRenderedBytes = 16 << 20

type Config struct {
	// Name is the display name of the wiki.
	Name string
	// URL is the base URL the server calls, with the context path if there is one (http://127.0.0.1:8080 for
	// the Docker image, http://host:8080/xwiki for the WAR in a servlet container).
	URL string
	// PublicURL is the base URL of the links given to users, URL when empty.
	PublicURL string
	// Wiki is the main (sub)wiki: page IDs without a "wiki:" prefix are in this wiki.
	Wiki       string
	HTTPClient *http.Client
	UserAgent  string
}

type Engine struct {
	cfg    Config
	base   string
	public string
	http   *http.Client
}

var _ wiki.Engine = (*Engine)(nil)

func New(cfg Config) (*Engine, error) {
	if cfg.URL == "" {
		return nil, errors.New("xwiki: url is required")
	}
	if cfg.Wiki == "" {
		cfg.Wiki = "xwiki"
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = cfg.URL
	}
	if cfg.Name == "" {
		cfg.Name = "XWiki"
	}
	hc := http.DefaultClient
	if cfg.HTTPClient != nil {
		hc = cfg.HTTPClient
	}
	// a redirect from a /bin action means the login page: report it instead of following it
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Engine{
		cfg:    cfg,
		base:   strings.TrimRight(cfg.URL, "/"),
		public: strings.TrimRight(cfg.PublicURL, "/"),
		http:   &client,
	}, nil
}

func (e *Engine) Info() wiki.Info {
	return wiki.Info{
		Engine:          "xwiki",
		Name:            e.cfg.Name,
		URL:             e.public,
		Markup:          "xwiki/2.1",
		PageIDHelp:      pageIDHelp,
		SearchHelp:      searchHelp,
		AttachmentsHelp: "the files attached to the page; get_attachment takes the file name.",
	}
}

const pageIDHelp = `Page IDs are XWiki document references: Space.Page, Space.Sub.Page (nested spaces separated by dots, a dot ` +
	`or colon inside a name escaped with a backslash, e.g. Release\.Notes). A nested page is Space.Sub.WebHome; an ID ` +
	`without .WebHome that does not exist is tried as a nested page too, and a single name Foo means Foo.WebHome. ` +
	`Pages of another subwiki are prefixed with its name: subwiki:Space.Page. Reuse the IDs returned by the tools.`

const searchHelp = `The query is a Solr query parsed by XWiki's extended DisMax parser. Plain words search the title and ` +
	`page name (boosted), the content, object properties and attachment content; several words are OR-ed and ranked, ` +
	`so use a few specific words, "quoted phrases" or AND for stricter matches. Syntax: "exact phrase", AND, OR, NOT ` +
	`or -word, +word (required), word* (prefix), (grouping), field:value. Useful fields: title:, doccontent: ` +
	`(rendered content), doccontentraw: (source), name: (page name), fullname:"Space.Page", space_exact:"Space.Sub" ` +
	`(pages directly in a space), space_prefix:"Space" (a space and everything below it, same as the under parameter), ` +
	`author:"xwiki:XWiki.jdoe" (last author), creator:, class:Blog.BlogPostClass (pages holding an object of that ` +
	`class), property.Blog.BlogPostClass.category:News (an object property), date:[NOW-7DAYS TO NOW] (last ` +
	`modified), creationdate:[2025-01-01T00:00:00Z TO *], locale:en. Escape + - && || ! ( ) { } [ ] ^ " ~ * ? : \ / ` +
	`with a backslash to search for them literally. Hidden pages are not returned; results have no text snippets, ` +
	`read the pages with get_page.`

// Login checks the credentials with a REST request and reads the authenticated user from the XWiki-User header.
func (e *Engine) Login(ctx context.Context, cred wiki.Credential) (*wiki.Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.restURL("/wikis/"+url.PathEscape(e.cfg.Wiki)), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cred.Username, cred.Secret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", e.cfg.UserAgent)
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("XWiki login check: %w", err)
	}
	defer drain(resp)

	user, known := requestUser(resp)
	switch {
	case !known:
		return nil, fmt.Errorf("XWiki login check: no %s header in the response (HTTP %d), XWiki 16.4 or newer is required",
			versionHeader, resp.StatusCode)
	case user == "":
		return nil, fmt.Errorf("%w: XWiki did not authenticate %q", wiki.ErrInvalidCredentials, cred.Username)
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("XWiki login check: HTTP %d", resp.StatusCode)
	}
	identity := &wiki.Identity{ID: user, Login: cred.Username}
	identity.DisplayName = e.userName(ctx, cred, user)
	return identity, nil
}

// userName reads "first_name last_name" from the user's profile, "" when it cannot.
func (e *Engine) userName(ctx context.Context, cred wiki.Credential, user string) string {
	ref, err := ParseReference(user, e.cfg.Wiki)
	if err != nil {
		return ""
	}
	var obj restObject
	if err := e.getJSON(ctx, cred, ref.restPath()+"/objects/XWiki.XWikiUsers/0", nil, &obj); err != nil {
		return ""
	}
	var first, last string
	for _, p := range obj.Properties {
		switch p.Name {
		case "first_name":
			first = p.Value
		case "last_name":
			last = p.Value
		}
	}
	return strings.TrimSpace(first + " " + last)
}

func (e *Engine) Session(cred wiki.Credential) wiki.Session {
	return &Session{e: e, cred: cred}
}

// viewURL is the link to the page for users.
func (e *Engine) viewURL(ref Reference) string {
	return e.public + ref.actionPath("view", e.cfg.Wiki)
}

// Session is the XWiki API of one user.
type Session struct {
	e    *Engine
	cred wiki.Credential
}

var (
	_ wiki.Session          = (*Session)(nil)
	_ wiki.AttachmentReader = (*Session)(nil)
)

func (s *Session) ref(id string) (Reference, error) {
	ref, err := ParseReference(id, s.e.cfg.Wiki)
	if err != nil {
		return ref, fmt.Errorf("%w: %v", wiki.ErrBadRequest, err)
	}
	return ref, nil
}

// resolve parses the page ID and returns the reference of the existing page, trying the nested page form too.
func (s *Session) resolve(ctx context.Context, id string) (Reference, error) {
	ref, err := s.ref(id)
	if err != nil {
		return ref, err
	}
	_, ref, err = withNested(ref, func(r Reference) (struct{}, error) {
		var p restPageSummary
		return struct{}{}, s.e.getJSON(ctx, s.cred, r.restPath(), nil, &p)
	})
	return ref, err
}

func (s *Session) id(ref Reference) string {
	return ref.ID(s.e.cfg.Wiki)
}

// withNested calls fn with ref and, when the page is not found and ref is a terminal page reference, again with
// the nested page form (Space.Page -> Space.Page.WebHome). It returns the reference that worked.
func withNested[T any](ref Reference, fn func(Reference) (T, error)) (T, Reference, error) {
	v, err := fn(ref)
	if errors.Is(err, wiki.ErrNotFound) {
		if nested, ok := ref.Nested(); ok {
			if v2, err2 := fn(nested); err2 == nil || !errors.Is(err2, wiki.ErrNotFound) {
				return v2, nested, err2
			}
		}
	}
	return v, ref, err
}

func (s *Session) Search(ctx context.Context, req wiki.SearchRequest) (*wiki.SearchResults, error) {
	q := strings.TrimSpace(req.Query)
	wikiName := s.e.cfg.Wiki
	if req.Under != "" {
		ref, err := s.ref(req.Under)
		if err != nil {
			return nil, err
		}
		wikiName = ref.Wiki
		q = "(" + q + ") AND space_prefix:" + solrQuote(ref.SubtreeSpace())
	}
	results, hasMore, err := s.solr(ctx, wikiName, q, req.Sort == wiki.SortNewest, req.Limit, req.Offset)
	if err != nil {
		return nil, err
	}
	out := &wiki.SearchResults{Hits: []wiki.SearchHit{}, Offset: req.Offset, HasMore: hasMore}
	seen := map[string]bool{}
	for _, r := range results {
		ref, err := ParseReference(r.PageFullName, r.Wiki)
		if err != nil {
			continue
		}
		id := s.id(ref)
		if seen[id] { // one hit per translation of a page
			continue
		}
		seen[id] = true
		out.Hits = append(out.Hits, wiki.SearchHit{
			ID:       id,
			Title:    titleOr(r.Title, ref),
			URL:      s.e.viewURL(ref),
			Location: ref.Space(),
			Score:    r.Score,
			Modified: r.Modified.Time,
			Author:   firstNonEmpty(r.AuthorName, r.Author),
		})
	}
	return out, nil
}

// solr runs a Solr query through the REST API; it asks for one hit more than limit to tell whether more follow.
func (s *Session) solr(ctx context.Context, wikiName, q string, newest bool, limit, offset int) ([]restSearchResult, bool, error) {
	limit = min(limit, maxRESTLimit)
	query := url.Values{
		"type":        {"solr"},
		"q":           {q},
		"number":      {strconv.Itoa(limit + 1)},
		"start":       {strconv.Itoa(offset)},
		"prettyNames": {"true"},
	}
	if newest {
		query.Set("orderField", "date")
		query.Set("order", "desc")
	}
	var res restSearchResults
	if err := s.e.getJSON(ctx, s.cred, "/wikis/"+url.PathEscape(wikiName)+"/query", query, &res); err != nil {
		if !errors.Is(err, wiki.ErrInvalidCredentials) && !errors.Is(err, wiki.ErrForbidden) {
			return nil, false, fmt.Errorf("%w: the search failed, check the Solr query syntax: %v", wiki.ErrBadRequest, err)
		}
		return nil, false, err
	}
	hits := res.SearchResults
	if len(hits) > limit {
		return hits[:limit], true, nil
	}
	return hits, false, nil
}

func (s *Session) GetPage(ctx context.Context, req wiki.PageRequest) (*wiki.Page, error) {
	ref, err := s.ref(req.ID)
	if err != nil {
		return nil, err
	}
	path := func(r Reference) string {
		if req.Version != "" {
			return r.restPath() + "/history/" + url.PathEscape(req.Version)
		}
		return r.restPath()
	}
	p, ref, err := withNested(ref, func(r Reference) (*restPage, error) {
		var p restPage
		return &p, s.e.getJSON(ctx, s.cred, path(r), url.Values{"prettyNames": {"true"}}, &p)
	})
	if err != nil {
		return nil, err
	}
	page := &wiki.Page{
		ID:       s.id(ref),
		Title:    titleOr(p.Title, ref),
		URL:      s.e.viewURL(ref),
		Location: ref.Space(),
		Version:  p.Version,
		Modified: p.Modified.Time,
		Author:   firstNonEmpty(p.ModifierName, p.Modifier),
		Markup:   p.Syntax,
		Format:   wiki.FormatSource,
		Content:  p.Content,
	}
	switch req.Format {
	case wiki.FormatText, wiki.FormatHTML:
		page.Format = req.Format
		page.Content, err = s.render(ctx, ref, req.Version, req.Format)
		if err != nil {
			return nil, err
		}
	}
	return page, nil
}

// render renders the page with the "get" action (the content without the skin).
func (s *Session) render(ctx context.Context, ref Reference, version string, format wiki.Format) (string, error) {
	// the get action renders HTML by default; outputSyntax takes the syntax type only ("plain/1.0" is ignored)
	query := url.Values{}
	if format == wiki.FormatText {
		query.Set("outputSyntax", "plain")
	}
	if version != "" {
		query.Set("rev", version)
	}
	resp, err := s.e.do(ctx, s.cred, call{method: http.MethodGet, url: s.e.base + ref.actionPath("get", s.e.cfg.Wiki), query: query})
	if err != nil {
		return "", err
	}
	defer drain(resp)
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRenderedBytes))
	if err != nil {
		return "", fmt.Errorf("XWiki rendering %s: %w", ref.Local(), err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *Session) ListPages(ctx context.Context, req wiki.ListRequest) (*wiki.PageList, error) {
	if req.Recursive {
		return s.listRecursive(ctx, req)
	}
	var pages restPages
	if req.Parent == "" {
		query := url.Values{"offset": {strconv.Itoa(req.Offset)}, "limit": {strconv.Itoa(req.Limit + 1)}}
		if err := s.e.getJSON(ctx, s.cred, "/wikis/"+url.PathEscape(s.e.cfg.Wiki)+"/children", query, &pages); err != nil {
			return nil, err
		}
		if len(pages.PageSummaries) == 0 && req.Offset == 0 {
			return s.childrenFromIndex(ctx, Reference{Wiki: s.e.cfg.Wiki}, req)
		}
	} else {
		ref, err := s.ref(req.Parent)
		if err != nil {
			return nil, err
		}
		query := url.Values{
			"hierarchy": {"nestedpages"},
			"start":     {strconv.Itoa(req.Offset)},
			"number":    {strconv.Itoa(req.Limit + 1)},
		}
		_, _, err = withNested(ref, func(r Reference) (struct{}, error) {
			return struct{}{}, s.e.getJSON(ctx, s.cred, r.restPath()+"/children", query, &pages)
		})
		if errors.Is(err, wiki.ErrNotFound) {
			// a space without a home page (common in wikis from before nested pages) still has children
			return s.childrenFromIndex(ctx, ref, req)
		}
		if err != nil {
			return nil, err
		}
	}
	items, hasMore := wiki.Window(pages.PageSummaries, 0, req.Limit)
	out := &wiki.PageList{Items: make([]wiki.PageItem, 0, len(items)), Offset: req.Offset, HasMore: hasMore}
	for _, ps := range items {
		ref, err := ParseReference(ps.FullName, ps.Wiki)
		if err != nil {
			continue
		}
		out.Items = append(out.Items, wiki.PageItem{ID: s.id(ref), Title: titleOr(ps.Title, ref), URL: s.e.viewURL(ref), Kind: wiki.KindPage})
	}
	return out, nil
}

// maxRESTLimit is the largest page size the REST API accepts (one hit more is asked for to detect more results).
const maxRESTLimit = 999

// indexChildrenScan bounds the Solr hits childrenFromIndex derives the children from.
const indexChildrenScan = maxRESTLimit

// childrenFromIndex derives the direct children of a page from the Solr index: the pages of its space and the
// nested pages of its child spaces, also when they have no home page. It reads at most indexChildrenScan pages
// below the parent, a page tree larger than that may be listed incompletely.
func (s *Session) childrenFromIndex(ctx context.Context, parent Reference, req wiki.ListRequest) (*wiki.PageList, error) {
	q, depth := "*:*", 0
	if len(parent.Spaces) > 0 {
		q = "space_prefix:" + solrQuote(parent.SubtreeSpace())
		if n, ok := parent.Nested(); ok {
			parent = n
		}
		depth = len(parent.Spaces)
	}
	results, _, err := s.solr(ctx, parent.Wiki, q, false, indexChildrenScan, 0)
	if err != nil {
		return nil, err
	}
	var items []wiki.PageItem
	index := map[string]int{}
	for _, r := range results {
		ref, err := ParseReference(r.PageFullName, r.Wiki)
		if err != nil || len(ref.Spaces) < depth {
			continue
		}
		child := ref // a terminal page in the parent's space
		if len(ref.Spaces) > depth {
			child = Reference{Wiki: ref.Wiki, Spaces: ref.Spaces[:depth+1], Name: webHome}
		} else if ref.Name == webHome {
			continue // the parent itself
		}
		title := titleOr("", child)
		if child.Absolute() == ref.Absolute() {
			title = titleOr(r.Title, ref)
		}
		if i, ok := index[child.Absolute()]; ok {
			if child.Absolute() == ref.Absolute() {
				items[i].Title = title
			}
			continue
		}
		index[child.Absolute()] = len(items)
		items = append(items, wiki.PageItem{ID: s.id(child), Title: title, URL: s.e.viewURL(child), Kind: wiki.KindPage})
	}
	slices.SortFunc(items, func(a, b wiki.PageItem) int { return strings.Compare(a.ID, b.ID) })
	window, hasMore := wiki.Window(items, req.Offset, req.Limit)
	return &wiki.PageList{Items: window, Offset: req.Offset, HasMore: hasMore}, nil
}

// listRecursive lists all pages below the parent with a Solr query on space_prefix.
func (s *Session) listRecursive(ctx context.Context, req wiki.ListRequest) (*wiki.PageList, error) {
	q, wikiName := "*:*", s.e.cfg.Wiki
	if req.Parent != "" {
		ref, err := s.ref(req.Parent)
		if err != nil {
			return nil, err
		}
		self := ref
		if n, ok := ref.Nested(); ok {
			self = n
		}
		wikiName = ref.Wiki
		q = "space_prefix:" + solrQuote(ref.SubtreeSpace()) + " AND -fullname:" + solrQuote(self.Local())
	}
	results, hasMore, err := s.solr(ctx, wikiName, q, false, req.Limit, req.Offset)
	if err != nil {
		return nil, err
	}
	out := &wiki.PageList{Items: []wiki.PageItem{}, Offset: req.Offset, HasMore: hasMore}
	seen := map[string]bool{}
	for _, r := range results {
		ref, err := ParseReference(r.PageFullName, r.Wiki)
		if err != nil || seen[ref.Absolute()] {
			continue
		}
		seen[ref.Absolute()] = true
		out.Items = append(out.Items, wiki.PageItem{
			ID: s.id(ref), Title: titleOr(r.Title, ref), URL: s.e.viewURL(ref), Kind: wiki.KindPage, Modified: r.Modified.Time,
		})
	}
	return out, nil
}

func (s *Session) History(ctx context.Context, req wiki.HistoryRequest) (*wiki.History, error) {
	// XWiki answers the history of a missing page with an empty list, not 404
	ref, err := s.resolve(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	query := url.Values{
		"start":       {strconv.Itoa(req.Offset)},
		"number":      {strconv.Itoa(req.Limit + 1)},
		"order":       {"desc"},
		"prettyNames": {"true"},
	}
	var h restHistory
	if err := s.e.getJSON(ctx, s.cred, ref.restPath()+"/history", query, &h); err != nil {
		return nil, err
	}
	items, hasMore := wiki.Window(h.HistorySummaries, 0, req.Limit)
	out := &wiki.History{ID: s.id(ref), Revisions: make([]wiki.Revision, 0, len(items)), Offset: req.Offset, HasMore: hasMore}
	for _, hs := range items {
		out.Revisions = append(out.Revisions, wiki.Revision{
			Version:  hs.Version,
			Modified: hs.Modified.Time,
			Author:   firstNonEmpty(hs.ModifierName, hs.Modifier),
			Summary:  hs.Comment,
		})
	}
	return out, nil
}

func (s *Session) RecentChanges(ctx context.Context, req wiki.ChangesRequest) (*wiki.Changes, error) {
	query := url.Values{
		"start":       {strconv.Itoa(req.Offset)},
		"number":      {strconv.Itoa(req.Limit + 1)},
		"order":       {"desc"},
		"prettyNames": {"true"},
	}
	if !req.Since.IsZero() {
		query.Set("date", strconv.FormatInt(req.Since.UnixMilli(), 10))
	}
	var h restHistory
	if err := s.e.getJSON(ctx, s.cred, "/wikis/"+url.PathEscape(s.e.cfg.Wiki)+"/modifications", query, &h); err != nil {
		return nil, err
	}
	items, hasMore := wiki.Window(h.HistorySummaries, 0, req.Limit)
	out := &wiki.Changes{Changes: make([]wiki.Change, 0, len(items)), Offset: req.Offset, HasMore: hasMore}
	for _, hs := range items {
		ref := referenceFromParts(firstNonEmpty(hs.Wiki, s.e.cfg.Wiki), hs.Space, hs.Name)
		out.Changes = append(out.Changes, wiki.Change{
			ID:       s.id(ref),
			URL:      s.e.viewURL(ref),
			Version:  hs.Version,
			Modified: hs.Modified.Time,
			Author:   firstNonEmpty(hs.ModifierName, hs.Modifier),
			Summary:  hs.Comment,
		})
	}
	return out, nil
}

// Backlinks finds the pages linking to the page in the Solr index (the links field, XWiki 14.8+).
func (s *Session) Backlinks(ctx context.Context, id string, limit int) ([]wiki.PageItem, error) {
	ref, err := s.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	q := "links:" + solrQuote("entity:document:"+ref.Absolute())
	results, _, err := s.solr(ctx, ref.Wiki, q, false, limit, 0)
	if err != nil {
		return nil, err
	}
	out := []wiki.PageItem{}
	seen := map[string]bool{}
	for _, r := range results {
		from, err := ParseReference(r.PageFullName, r.Wiki)
		if err != nil || seen[from.Absolute()] {
			continue
		}
		seen[from.Absolute()] = true
		out = append(out, wiki.PageItem{ID: s.id(from), Title: titleOr(r.Title, from), URL: s.e.viewURL(from), Kind: wiki.KindPage})
	}
	return out, nil
}

func (s *Session) Attachments(ctx context.Context, pageID string) ([]wiki.Attachment, error) {
	// like the history, the attachments of a missing page are an empty list
	ref, err := s.resolve(ctx, pageID)
	if err != nil {
		return nil, err
	}
	var atts restAttachments
	if err := s.e.getJSON(ctx, s.cred, ref.restPath()+"/attachments", url.Values{"prettyNames": {"true"}}, &atts); err != nil {
		return nil, err
	}
	out := make([]wiki.Attachment, 0, len(atts.Attachments))
	for _, a := range atts.Attachments {
		out = append(out, s.attachment(a))
	}
	return out, nil
}

func (s *Session) attachment(a restAttachment) wiki.Attachment {
	size := a.LongSize
	if size == 0 {
		size = a.Size
	}
	return wiki.Attachment{
		Name:     a.Name,
		Size:     size,
		MimeType: a.MimeType,
		Modified: a.Date.Time,
		Author:   firstNonEmpty(a.AuthorName, a.Author),
		Version:  a.Version,
		URL:      s.e.publicURL(a.XWikiAbsoluteURL),
	}
}

// publicURL rewrites an absolute URL generated by XWiki (from its own idea of its address, e.g. the internal one
// the server calls) to the public base URL.
func (e *Engine) publicURL(u string) string {
	if u == "" || e.public == e.base {
		return u
	}
	pu, err := url.Parse(u)
	if err != nil {
		return u
	}
	base, err := url.Parse(e.base)
	if err != nil || pu.Host != base.Host {
		return u
	}
	return e.public + strings.TrimPrefix(pu.EscapedPath(), base.EscapedPath())
}

func (s *Session) Attachment(ctx context.Context, pageID, name string, maxBytes int64) (*wiki.AttachmentData, error) {
	ref, err := s.resolve(ctx, pageID)
	if err != nil {
		return nil, err
	}
	all, err := s.Attachments(ctx, s.id(ref))
	if err != nil {
		return nil, err
	}
	var meta *wiki.Attachment
	for i := range all {
		if all[i].Name == name {
			meta = &all[i]
		}
	}
	if meta == nil {
		return nil, fmt.Errorf("%w: no attachment %q on %s", wiki.ErrNotFound, name, s.id(ref))
	}
	resp, err := s.e.do(ctx, s.cred, call{method: http.MethodGet, url: s.e.restURL(ref.restPath() + "/attachments/" + url.PathEscape(name))})
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("XWiki reading attachment %s: %w", name, err)
	}
	out := &wiki.AttachmentData{Attachment: *meta, Data: data}
	if int64(len(data)) > maxBytes {
		out.Data, out.Truncated = data[:maxBytes], true
	}
	if out.MimeType == "" {
		out.MimeType = resp.Header.Get("Content-Type")
	}
	return out, nil
}

func solrQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// titleOr returns the title, or for a page without one the name a user sees: the page name, or the space name of
// a nested page.
func titleOr(title string, ref Reference) string {
	if title != "" {
		return title
	}
	if ref.Name == webHome && len(ref.Spaces) > 0 {
		return ref.Spaces[len(ref.Spaces)-1]
	}
	return ref.Name
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
