// Package dokuwiki is the DokuWiki engine: it calls the JSON-RPC remote API (lib/exe/jsonrpc.php, DokuWiki
// 2024-02-06 "Kaos" or newer) as the signed-in user, authenticated with HTTP basic auth or, when the user signs in
// with a token from their DokuWiki profile, with that token as a bearer token.
//
// The wiki needs the remote API enabled (config "remote") and the users allowed to use it (config "remoteuser",
// e.g. "@user").
package dokuwiki

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

type Config struct {
	// Name is the display name of the wiki.
	Name string
	// URL is the base URL of DokuWiki the server calls (the directory of doku.php).
	URL string
	// PublicURL is the base URL of the links given to users, URL when empty.
	PublicURL  string
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
		return nil, errors.New("dokuwiki: url is required")
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = cfg.URL
	}
	if cfg.Name == "" {
		cfg.Name = "DokuWiki"
	}
	hc := http.DefaultClient
	if cfg.HTTPClient != nil {
		hc = cfg.HTTPClient
	}
	return &Engine{
		cfg:    cfg,
		base:   strings.TrimRight(cfg.URL, "/"),
		public: strings.TrimRight(cfg.PublicURL, "/"),
		http:   hc,
	}, nil
}

func (e *Engine) Info() wiki.Info {
	return wiki.Info{
		Engine:     "dokuwiki",
		Name:       e.cfg.Name,
		URL:        e.public,
		Markup:     "dokuwiki",
		PageIDHelp: pageIDHelp,
		SearchHelp: searchHelp,
		AttachmentsHelp: "the media files in the namespace of the page (DokuWiki has no per-page attachments); " +
			"get_attachment takes a media ID (namespace:file.pdf) or a file name in that namespace.",
	}
}

const pageIDHelp = `Page IDs are DokuWiki page IDs: namespace:subnamespace:page in lower case (e.g. wiki:syntax), the ` +
	`main page of a namespace is usually namespace:start. A namespace ID is the same without the page (wiki). ` +
	`Reuse the IDs returned by the tools.`

const searchHelp = `The query uses the DokuWiki full-text search syntax: all words must occur on the page (AND), ` +
	`"exact phrase", -word excludes, word* / *word / *word* are wildcards, @namespace limits the search to a ` +
	`namespace and ^namespace excludes one (the under parameter adds @namespace for you). Words shorter than 3 ` +
	`characters are not indexed. Only page text is searched; snippets come with the first 15 hits.`

// call invokes a JSON-RPC method as the user of cred and decodes its result into out.
func (e *Engine) call(ctx context.Context, cred wiki.Credential, method string, params, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+"/lib/exe/jsonrpc.php", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", e.cfg.UserAgent)
	if isToken(cred.Secret) {
		req.Header.Set("Authorization", "Bearer "+cred.Secret)
	} else {
		req.SetBasicAuth(cred.Username, cred.Secret)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("DokuWiki %s: %w", method, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	var res struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&res)
	if decodeErr != nil || (res.Error != nil && res.Error.Code != 0) {
		code, msg := 0, ""
		if res.Error != nil {
			code, msg = res.Error.Code, res.Error.Message
		}
		return apiError(method, resp.StatusCode, code, msg, decodeErr)
	}
	if resp.StatusCode >= 300 {
		return apiError(method, resp.StatusCode, 0, "", nil)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(res.Result, out); err != nil {
		return fmt.Errorf("DokuWiki %s: decoding the result: %w", method, err)
	}
	return nil
}

func apiError(method string, status, code int, msg string, decodeErr error) error {
	what := fmt.Sprintf("DokuWiki %s: HTTP %d", method, status)
	if code != 0 {
		what += fmt.Sprintf(", error %d: %s", code, strings.ReplaceAll(msg, "\n", " "))
	} else if decodeErr != nil {
		what += ", the response is not JSON-RPC"
	}
	switch {
	case status == http.StatusUnauthorized || code == -32603:
		// DokuWiki answers 401 only when no user is logged in: the credentials were not accepted
		return fmt.Errorf("%w (%s)", wiki.ErrInvalidCredentials, what)
	case status == http.StatusForbidden || code == -32604 || code == 111 || code == 211 || code == 212 || code == 114:
		return fmt.Errorf("%w (%s)", wiki.ErrForbidden, what)
	case code == 121 || code == 221:
		return fmt.Errorf("%w (%s)", wiki.ErrNotFound, what)
	case code == -32605 || (status == http.StatusNotFound && code == 0):
		return fmt.Errorf("%s: the remote API is disabled, enable the DokuWiki options remote and remoteuser", what)
	case status == http.StatusBadRequest || code == 131 || code == 231:
		return fmt.Errorf("%w (%s)", wiki.ErrBadRequest, what)
	}
	return errors.New(what)
}

var jwtRe = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

// isToken tells a DokuWiki token (a JWT from the user's profile) from a password.
func isToken(secret string) bool {
	return jwtRe.MatchString(secret)
}

type apiUser struct {
	Login   string   `json:"login"`
	Name    string   `json:"name"`
	Mail    string   `json:"mail"`
	Groups  []string `json:"groups"`
	IsAdmin bool     `json:"isadmin"`
}

// Login checks the credentials with core.whoAmI.
func (e *Engine) Login(ctx context.Context, cred wiki.Credential) (*wiki.Identity, error) {
	var u apiUser
	if err := e.call(ctx, cred, "core.whoAmI", map[string]any{}, &u); err != nil {
		if errors.Is(err, wiki.ErrForbidden) {
			// whoAmI is allowed to every logged in user: forbidden means the user may not use the remote API
			return nil, fmt.Errorf("%w: %q may not use the DokuWiki remote API (option remoteuser)", wiki.ErrInvalidCredentials, cred.Username)
		}
		return nil, err
	}
	if u.Login == "" {
		return nil, fmt.Errorf("%w: DokuWiki did not authenticate %q", wiki.ErrInvalidCredentials, cred.Username)
	}
	// with a token the login comes from the token, not from what the user typed
	return &wiki.Identity{ID: u.Login, Login: u.Login, DisplayName: u.Name}, nil
}

func (e *Engine) Session(cred wiki.Credential) wiki.Session {
	return &Session{e: e, cred: cred}
}

func (e *Engine) pageURL(id string) string {
	return e.public + "/doku.php?id=" + url.QueryEscape(id)
}

func (e *Engine) mediaURL(id string) string {
	return e.public + "/lib/exe/fetch.php?media=" + url.QueryEscape(id)
}

// Session is the DokuWiki API of one user.
type Session struct {
	e    *Engine
	cred wiki.Credential
}

var (
	_ wiki.Session          = (*Session)(nil)
	_ wiki.AttachmentReader = (*Session)(nil)
)

// API response types (inc/Remote/Response).

type apiPage struct {
	ID         string `json:"id"`
	Revision   int64  `json:"revision"`
	Size       int64  `json:"size"`
	Title      string `json:"title"`
	Permission int    `json:"permission"`
	Author     string `json:"author"`
}

type apiPageHit struct {
	apiPage
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet"`
}

type apiPageChange struct {
	ID         string `json:"id"`
	Revision   int64  `json:"revision"`
	Author     string `json:"author"`
	Summary    string `json:"summary"`
	Type       string `json:"type"`
	SizeChange int64  `json:"sizechange"`
}

type apiMedia struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Size     int64  `json:"size"`
	IsImage  bool   `json:"isimage"`
	Author   string `json:"author"`
}

func unix(ts int64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0).UTC()
}

func cleanID(id string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(id)), ":")
}

// changeTypes are the DokuWiki change log types.
var changeTypes = map[string]string{"C": "created", "E": "edited", "e": "minor edit", "D": "deleted", "R": "reverted"}

func changeType(t string) string {
	return cmp.Or(changeTypes[t], t)
}

func (s *Session) Search(ctx context.Context, req wiki.SearchRequest) (*wiki.SearchResults, error) {
	q := strings.TrimSpace(req.Query)
	if ns := cleanID(req.Under); ns != "" {
		q += " @" + ns
	}
	var hits []apiPageHit
	if err := s.e.call(ctx, s.cred, "core.searchPages", map[string]any{"query": q}, &hits); err != nil {
		return nil, err
	}
	if req.Sort == wiki.SortNewest {
		slices.SortStableFunc(hits, func(a, b apiPageHit) int { return cmp.Compare(b.Revision, a.Revision) })
	}
	window, hasMore := wiki.Window(hits, req.Offset, req.Limit)
	out := &wiki.SearchResults{Hits: make([]wiki.SearchHit, 0, len(window)), Offset: req.Offset, HasMore: hasMore, Total: len(hits)}
	for _, h := range window {
		out.Hits = append(out.Hits, wiki.SearchHit{
			ID:       h.ID,
			Title:    cmp.Or(h.Title, h.ID),
			URL:      s.e.pageURL(h.ID),
			Location: namespace(h.ID),
			Snippet:  wiki.HTMLToText(h.Snippet),
			Score:    h.Score,
			Modified: unix(h.Revision),
		})
	}
	return out, nil
}

func (s *Session) pageInfo(ctx context.Context, id string, rev int64) (*apiPage, error) {
	var p apiPage
	err := s.e.call(ctx, s.cred, "core.getPageInfo", map[string]any{"page": id, "rev": rev, "author": true}, &p)
	return &p, err
}

func parseRev(version string) (int64, error) {
	if version == "" {
		return 0, nil
	}
	rev, err := strconv.ParseInt(version, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: a DokuWiki version is a revision timestamp, got %q", wiki.ErrBadRequest, version)
	}
	return rev, nil
}

func (s *Session) GetPage(ctx context.Context, req wiki.PageRequest) (*wiki.Page, error) {
	id := cleanID(req.ID)
	rev, err := parseRev(req.Version)
	if err != nil {
		return nil, err
	}
	info, err := s.pageInfo(ctx, id, rev)
	if err != nil {
		return nil, err
	}
	page := &wiki.Page{
		ID:       info.ID,
		Title:    cmp.Or(info.Title, info.ID),
		URL:      s.e.pageURL(info.ID),
		Location: namespace(info.ID),
		Version:  strconv.FormatInt(info.Revision, 10),
		Modified: unix(info.Revision),
		Author:   info.Author,
		Markup:   "dokuwiki",
		Format:   wiki.FormatSource,
	}
	switch req.Format {
	case wiki.FormatText, wiki.FormatHTML:
		var html string
		if err := s.e.call(ctx, s.cred, "core.getPageHTML", map[string]any{"page": id, "rev": rev}, &html); err != nil {
			return nil, err
		}
		page.Format, page.Content = req.Format, html
		if req.Format == wiki.FormatText {
			page.Content = wiki.HTMLToText(html)
		}
	default:
		if err := s.e.call(ctx, s.cred, "core.getPage", map[string]any{"page": id, "rev": rev}, &page.Content); err != nil {
			return nil, err
		}
	}
	return page, nil
}

// ListPages lists the pages and subnamespaces of a namespace. DokuWiki has no call for namespaces, they are
// derived from all pages below the parent.
func (s *Session) ListPages(ctx context.Context, req wiki.ListRequest) (*wiki.PageList, error) {
	ns := cleanID(req.Parent)
	var pages []apiPage
	if err := s.e.call(ctx, s.cred, "core.listPages", map[string]any{"namespace": ns, "depth": 0}, &pages); err != nil {
		return nil, err
	}
	slices.SortFunc(pages, func(a, b apiPage) int { return cmp.Compare(a.ID, b.ID) })

	prefix := ""
	if ns != "" {
		prefix = ns + ":"
	}
	var items []wiki.PageItem
	seenNS := map[string]bool{}
	for _, p := range pages {
		rest, ok := strings.CutPrefix(p.ID, prefix)
		if !ok {
			continue
		}
		sub, _, nested := strings.Cut(rest, ":")
		if !req.Recursive && nested {
			if child := prefix + sub; !seenNS[child] {
				seenNS[child] = true
				items = append(items, wiki.PageItem{ID: child, Title: sub, URL: s.e.pageURL(child + ":"), Kind: wiki.KindNamespace})
			}
			continue
		}
		items = append(items, wiki.PageItem{
			ID: p.ID, Title: cmp.Or(p.Title, p.ID), URL: s.e.pageURL(p.ID), Kind: wiki.KindPage, Modified: unix(p.Revision),
		})
	}
	window, hasMore := wiki.Window(items, req.Offset, req.Limit)
	return &wiki.PageList{Items: window, Offset: req.Offset, HasMore: hasMore}, nil
}

func (s *Session) History(ctx context.Context, req wiki.HistoryRequest) (*wiki.History, error) {
	id := cleanID(req.ID)
	var changes []apiPageChange
	if err := s.e.call(ctx, s.cred, "core.getPageHistory", map[string]any{"page": id, "first": req.Offset}, &changes); err != nil {
		return nil, err
	}
	window, hasMore := wiki.Window(changes, 0, req.Limit)
	out := &wiki.History{ID: id, Revisions: make([]wiki.Revision, 0, len(window)), Offset: req.Offset, HasMore: hasMore}
	for _, c := range window {
		out.Revisions = append(out.Revisions, wiki.Revision{
			Version:  strconv.FormatInt(c.Revision, 10),
			Modified: unix(c.Revision),
			Author:   c.Author,
			Summary:  c.Summary,
			Type:     changeType(c.Type),
		})
	}
	return out, nil
}

func (s *Session) RecentChanges(ctx context.Context, req wiki.ChangesRequest) (*wiki.Changes, error) {
	var ts int64
	if !req.Since.IsZero() {
		ts = req.Since.Unix()
	}
	var changes []apiPageChange
	if err := s.e.call(ctx, s.cred, "core.getRecentPageChanges", map[string]any{"timestamp": ts}, &changes); err != nil {
		return nil, err
	}
	slices.SortStableFunc(changes, func(a, b apiPageChange) int { return cmp.Compare(b.Revision, a.Revision) })
	window, hasMore := wiki.Window(changes, req.Offset, req.Limit)
	out := &wiki.Changes{Changes: make([]wiki.Change, 0, len(window)), Offset: req.Offset, HasMore: hasMore}
	for _, c := range window {
		out.Changes = append(out.Changes, wiki.Change{
			ID:       c.ID,
			URL:      s.e.pageURL(c.ID),
			Version:  strconv.FormatInt(c.Revision, 10),
			Modified: unix(c.Revision),
			Author:   c.Author,
			Summary:  c.Summary,
			Type:     changeType(c.Type),
		})
	}
	return out, nil
}

func (s *Session) Backlinks(ctx context.Context, id string, limit int) ([]wiki.PageItem, error) {
	var ids []string
	if err := s.e.call(ctx, s.cred, "core.getPageBackLinks", map[string]any{"page": cleanID(id)}, &ids); err != nil {
		return nil, err
	}
	ids, _ = wiki.Window(ids, 0, limit)
	out := make([]wiki.PageItem, 0, len(ids))
	for _, b := range ids {
		out = append(out, wiki.PageItem{ID: b, URL: s.e.pageURL(b), Kind: wiki.KindPage})
	}
	return out, nil
}

// Attachments lists the media files in the namespace of the page (DokuWiki has no per-page attachments).
func (s *Session) Attachments(ctx context.Context, pageID string) ([]wiki.Attachment, error) {
	var media []apiMedia
	if err := s.e.call(ctx, s.cred, "core.listMedia", map[string]any{"namespace": namespace(cleanID(pageID)), "depth": 1}, &media); err != nil {
		return nil, err
	}
	out := make([]wiki.Attachment, 0, len(media))
	for _, m := range media {
		out = append(out, s.attachment(m))
	}
	return out, nil
}

func (s *Session) attachment(m apiMedia) wiki.Attachment {
	return wiki.Attachment{
		Name:     m.ID,
		Size:     m.Size,
		MimeType: mime.TypeByExtension(path.Ext(m.ID)),
		Modified: unix(m.Revision),
		Author:   m.Author,
		Version:  strconv.FormatInt(m.Revision, 10),
		URL:      s.e.mediaURL(m.ID),
	}
}

// Attachment reads a media file; name is a media ID or a file name in the namespace of the page.
func (s *Session) Attachment(ctx context.Context, pageID, name string, maxBytes int64) (*wiki.AttachmentData, error) {
	id := cleanID(name)
	if !strings.Contains(id, ":") {
		if ns := namespace(cleanID(pageID)); ns != "" {
			id = ns + ":" + id
		}
	}
	var m apiMedia
	if err := s.e.call(ctx, s.cred, "core.getMediaInfo", map[string]any{"media": id, "author": true}, &m); err != nil {
		return nil, err
	}
	out := &wiki.AttachmentData{Attachment: s.attachment(m)}
	// the API returns the whole file base64 encoded, skip files far over the limit
	if m.Size > maxBytes*4 {
		out.Truncated = true
		return out, nil
	}
	var b64 string
	if err := s.e.call(ctx, s.cred, "core.getMedia", map[string]any{"media": id}, &b64); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("DokuWiki core.getMedia %s: %w", id, err)
	}
	out.Data = data
	if int64(len(data)) > maxBytes {
		out.Data, out.Truncated = data[:maxBytes], true
	}
	return out, nil
}

// namespace returns the namespace of a page ID, "" for the root.
func namespace(id string) string {
	if i := strings.LastIndexByte(id, ':'); i >= 0 {
		return id[:i]
	}
	return ""
}
