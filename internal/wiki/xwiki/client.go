package xwiki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

// REST responses of XWiki 16.2+ carry the version and the user XWiki authenticated the request as. On wrong basic
// auth credentials XWiki falls back to the guest user instead of answering 401 and leaves the user header out, so
// the headers are how a bad password is detected.
const (
	userHeader    = "XWiki-User"
	versionHeader = "XWiki-Version"
)

// requestUser returns the user of a REST response; known is false when the response does not tell (a /bin action
// or XWiki older than 16.2). An empty user is the guest.
func requestUser(resp *http.Response) (user string, known bool) {
	if resp.Header.Get(versionHeader) == "" {
		return "", false
	}
	user = resp.Header.Get(userHeader)
	if isGuest(user) {
		user = ""
	}
	return user, true
}

// errorBodyLimit is how much of an error response is quoted in the error message.
const errorBodyLimit = 400

// call is one HTTP request to XWiki as the user of cred.
type call struct {
	method      string
	url         string
	query       url.Values
	body        []byte
	contentType string
	accept      string
}

func (e *Engine) do(ctx context.Context, cred wiki.Credential, c call) (*http.Response, error) {
	u := c.url
	if len(c.query) > 0 {
		u += "?" + c.query.Encode()
	}
	var body io.Reader
	if c.body != nil {
		body = bytes.NewReader(c.body)
	}
	req, err := http.NewRequestWithContext(ctx, c.method, u, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cred.Username, cred.Secret)
	req.Header.Set("User-Agent", e.cfg.UserAgent)
	if c.accept != "" {
		req.Header.Set("Accept", c.accept)
	}
	if c.contentType != "" {
		req.Header.Set("Content-Type", c.contentType)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("XWiki %s %s: %w", c.method, redact(c.url), err)
	}
	if user, known := requestUser(resp); known && user == "" {
		drain(resp)
		return nil, fmt.Errorf("%w: XWiki treats the request as the guest user", wiki.ErrInvalidCredentials)
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer drain(resp)
	return nil, statusError(c, resp)
}

func (e *Engine) getJSON(ctx context.Context, cred wiki.Credential, path string, query url.Values, out any) error {
	resp, err := e.do(ctx, cred, call{method: http.MethodGet, url: e.restURL(path), query: query, accept: "application/json"})
	if err != nil {
		return err
	}
	defer drain(resp)
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("XWiki GET %s: decoding the response: %w", path, err)
	}
	return nil
}

func (e *Engine) restURL(path string) string {
	return e.base + "/rest" + path
}

func statusError(c call, resp *http.Response) error {
	snippet := readSnippet(resp.Body)
	what := fmt.Sprintf("XWiki %s %s: HTTP %d", c.method, redact(c.url), resp.StatusCode)
	if snippet != "" {
		what += ": " + snippet
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w (%s)", wiki.ErrNotFound, what)
	case http.StatusUnauthorized, http.StatusForbidden:
		// XWiki answers 401 also to an authenticated user without the right to see a page
		return fmt.Errorf("%w (%s)", wiki.ErrForbidden, what)
	case http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect:
		// the /bin actions redirect to the login page when the user may not see the page
		return fmt.Errorf("%w (%s, redirected to %s)", wiki.ErrForbidden, what, resp.Header.Get("Location"))
	case http.StatusBadRequest:
		return fmt.Errorf("%w (%s)", wiki.ErrBadRequest, what)
	}
	return fmt.Errorf("%s", what)
}

var (
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// readSnippet returns the start of an error body as one line of text (XWiki errors are often HTML pages or Java
// stack traces).
func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 4096))
	s := tagRe.ReplaceAllString(string(b), " ")
	s = strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
	if len(s) > errorBodyLimit {
		s = s[:errorBodyLimit] + "..."
	}
	return s
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// redact removes the query from a URL for error messages (queries may hold search terms of users).
func redact(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

func isGuest(user string) bool {
	return user == "" || user == "null" || strings.HasSuffix(user, "XWikiGuest")
}

// xtime decodes the dates of the REST API: milliseconds since the epoch in JSON, an ISO 8601 string in some
// versions.
type xtime struct{ time.Time }

func (t *xtime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		t.Time = time.UnixMilli(ms).UTC()
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700"} {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v.UTC()
			return nil
		}
	}
	return nil // an unknown date format is not worth failing the call
}

// REST model (JSON representation of xwiki.rest.model.xsd), only the fields used here.

type restPageSummary struct {
	ID               string `json:"id"`
	FullName         string `json:"fullName"`
	Wiki             string `json:"wiki"`
	Space            string `json:"space"`
	Name             string `json:"name"`
	Title            string `json:"title"`
	Version          string `json:"version"`
	XWikiAbsoluteURL string `json:"xwikiAbsoluteUrl"`
	Syntax           string `json:"syntax"`
}

type restPage struct {
	restPageSummary
	Modified     xtime  `json:"modified"`
	Modifier     string `json:"modifier"`
	ModifierName string `json:"modifierName"`
	Content      string `json:"content"`
	Comment      string `json:"comment"`
}

type restPages struct {
	PageSummaries []restPageSummary `json:"pageSummaries"`
}

type restSearchResult struct {
	Type         string  `json:"type"`
	PageFullName string  `json:"pageFullName"`
	Title        string  `json:"title"`
	Wiki         string  `json:"wiki"`
	Space        string  `json:"space"`
	PageName     string  `json:"pageName"`
	Modified     xtime   `json:"modified"`
	Author       string  `json:"author"`
	AuthorName   string  `json:"authorName"`
	Version      string  `json:"version"`
	Language     string  `json:"language"`
	Score        float64 `json:"score"`
}

type restSearchResults struct {
	SearchResults []restSearchResult `json:"searchResults"`
}

type restHistorySummary struct {
	PageID       string `json:"pageId"`
	Wiki         string `json:"wiki"`
	Space        string `json:"space"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Modified     xtime  `json:"modified"`
	Modifier     string `json:"modifier"`
	ModifierName string `json:"modifierName"`
	Language     string `json:"language"`
	Comment      string `json:"comment"`
}

type restHistory struct {
	HistorySummaries []restHistorySummary `json:"historySummaries"`
}

type restAttachment struct {
	Name             string `json:"name"`
	Size             int64  `json:"size"`
	LongSize         int64  `json:"longSize"`
	Version          string `json:"version"`
	MimeType         string `json:"mimeType"`
	Author           string `json:"author"`
	AuthorName       string `json:"authorName"`
	Date             xtime  `json:"date"`
	XWikiAbsoluteURL string `json:"xwikiAbsoluteUrl"`
}

type restAttachments struct {
	Attachments []restAttachment `json:"attachments"`
}

type restProperty struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type restObject struct {
	ClassName  string         `json:"className"`
	Number     int            `json:"number"`
	Headline   string         `json:"headline"`
	Properties []restProperty `json:"properties"`
}
