package mcpserver_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kinjelom/wiki-mcp/internal/mcpserver"
	"github.com/kinjelom/wiki-mcp/internal/oauth"
	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

// fakeEngine accepts jdoe/secret until the password is "changed".
type fakeEngine struct {
	passwordChanged atomic.Bool
}

func (e *fakeEngine) Info() wiki.Info {
	return wiki.Info{Engine: "fake", Name: "Fake Wiki", URL: "https://wiki.example.com", Markup: "fake",
		PageIDHelp: "IDs are words.", SearchHelp: "Words.", AttachmentsHelp: "files."}
}

func (e *fakeEngine) Login(_ context.Context, c wiki.Credential) (*wiki.Identity, error) {
	if c.Username != "jdoe" || c.Secret != "secret" || e.passwordChanged.Load() {
		return nil, wiki.ErrInvalidCredentials
	}
	return &wiki.Identity{ID: "XWiki.jdoe", Login: "jdoe", DisplayName: "John Doe"}, nil
}

func (e *fakeEngine) Session(c wiki.Credential) wiki.Session { return &fakeSession{e: e, cred: c} }

type fakeSession struct {
	e    *fakeEngine
	cred wiki.Credential
}

func (s *fakeSession) check() error {
	if s.e.passwordChanged.Load() {
		return wiki.ErrInvalidCredentials
	}
	return nil
}

func (s *fakeSession) Search(_ context.Context, r wiki.SearchRequest) (*wiki.SearchResults, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	return &wiki.SearchResults{Hits: []wiki.SearchHit{{ID: "deploy", Title: "Deploy as " + s.cred.Username, Snippet: r.Query}}}, nil
}

func (s *fakeSession) GetPage(_ context.Context, r wiki.PageRequest) (*wiki.Page, error) {
	if r.ID != "deploy" {
		return nil, wiki.ErrNotFound
	}
	return &wiki.Page{ID: "deploy", Title: "Deploy", Format: wiki.FormatSource, Content: strings.Repeat("abcdefghij", 10)}, nil
}

func (s *fakeSession) ListPages(context.Context, wiki.ListRequest) (*wiki.PageList, error) {
	return &wiki.PageList{Items: []wiki.PageItem{}}, nil
}

func (s *fakeSession) History(context.Context, wiki.HistoryRequest) (*wiki.History, error) {
	return &wiki.History{Revisions: []wiki.Revision{}}, nil
}

func (s *fakeSession) RecentChanges(context.Context, wiki.ChangesRequest) (*wiki.Changes, error) {
	return &wiki.Changes{Changes: []wiki.Change{}}, nil
}

func (s *fakeSession) Backlinks(context.Context, string, int) ([]wiki.PageItem, error) {
	return []wiki.PageItem{}, nil
}

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	engine *fakeEngine
	// noRedirect captures redirects of the authorization endpoint instead of following them
	noRedirect *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	store, err := oauth.OpenStore(t.TempDir() + "/wiki-mcp.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine := &fakeEngine{}
	reg := prometheus.NewRegistry()
	as, err := oauth.New(oauth.Config{
		Issuer:              srv.URL,
		Resource:            srv.URL + "/mcp",
		WikiName:            "Fake Wiki",
		AccessTokenTTL:      time.Hour,
		RefreshTokenTTL:     24 * time.Hour,
		SessionMaxAge:       48 * time.Hour,
		RefreshReuseGrace:   0,
		DynamicRegistration: true,
		ClientTTL:           time.Hour,
	}, store, make([]byte, oauth.KeySize), engine, logger, reg)
	if err != nil {
		t.Fatal(err)
	}
	server := mcpserver.New(mcpserver.Options{Engine: engine, Version: "test", Revoke: as.RevokeGrant, Logger: logger, Registerer: reg})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	if err := as.Register(mux); err != nil {
		t.Fatal(err)
	}
	mux.Handle("/mcp", as.Protect(handler))
	srv.Config.Handler = mux
	return &harness{t: t, srv: srv, engine: engine, noRedirect: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (h *harness) postForm(path string, form url.Values) (*http.Response, map[string]any) {
	h.t.Helper()
	resp, err := h.noRedirect.PostForm(h.srv.URL+path, form)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(resp.Body).Decode(&body)
	}
	return resp, body
}

func (h *harness) getJSON(path string) map[string]any {
	h.t.Helper()
	resp, err := http.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		h.t.Fatal(err)
	}
	return m
}

var requestRe = regexp.MustCompile(`name="request" value="([^"]+)"`)

// signIn runs the authorization code flow with PKCE and returns the token response.
func (h *harness) signIn(clientID, redirectURI string) map[string]any {
	h.t.Helper()
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {"xyz"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"scope":                 {"wiki offline_access"},
		"resource":              {h.srv.URL + "/mcp"},
	}
	resp, err := http.Get(h.srv.URL + "/authorize?" + q.Encode())
	if err != nil {
		h.t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := requestRe.FindSubmatch(page)
	if resp.StatusCode != http.StatusOK || m == nil {
		h.t.Fatalf("authorize page: HTTP %d\n%s", resp.StatusCode, page)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		h.t.Error("the sign-in page may be framed")
	}

	// a wrong password shows the form again
	resp, err = h.noRedirect.PostForm(h.srv.URL+"/authorize", url.Values{"request": {string(m[1])}, "username": {"jdoe"}, "password": {"nope"}, "action": {"allow"}})
	if err != nil {
		h.t.Fatal(err)
	}
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "Wrong user name or password") {
		h.t.Fatalf("wrong password: HTTP %d\n%s", resp.StatusCode, page)
	}

	resp, _ = h.postForm("/authorize", url.Values{"request": {string(m[1])}, "username": {"jdoe"}, "password": {"secret"}, "action": {"allow"}})
	if resp.StatusCode != http.StatusFound {
		h.t.Fatalf("sign-in: HTTP %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), redirectURI) || loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != h.srv.URL {
		h.t.Fatalf("redirect = %s", loc)
	}
	code := loc.Query().Get("code")

	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {clientID}, "code_verifier": {verifier}}
	resp, tok := h.postForm("/token", exchange)
	if resp.StatusCode != http.StatusOK || tok["access_token"] == nil || tok["refresh_token"] == nil {
		h.t.Fatalf("token exchange: HTTP %d %v", resp.StatusCode, tok)
	}
	if resp, body := h.postForm("/token", exchange); resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_grant" {
		h.t.Errorf("second use of a code: HTTP %d %v", resp.StatusCode, body)
	}
	return tok
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (h *harness) connect(token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// discovery: 401 with the resource metadata, which points at the authorization server
	resp, err := http.Post(h.srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+h.srv.URL+`/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("unauthenticated: HTTP %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	prm := h.getJSON("/.well-known/oauth-protected-resource/mcp")
	if prm["resource"] != h.srv.URL+"/mcp" || prm["authorization_servers"].([]any)[0] != h.srv.URL {
		t.Errorf("resource metadata = %v", prm)
	}
	asm := h.getJSON("/.well-known/oauth-authorization-server")
	if asm["client_id_metadata_document_supported"] != true || !strings.Contains(asm["token_endpoint_auth_methods_supported"].(any).([]any)[0].(string), "none") ||
		asm["registration_endpoint"] != h.srv.URL+"/register" {
		t.Errorf("authorization server metadata = %v", asm)
	}

	// dynamic registration: a redirect outside the allowed list is refused
	reg := func(redirect string) (*http.Response, map[string]any) {
		resp, err := http.Post(h.srv.URL+"/register", "application/json",
			strings.NewReader(`{"redirect_uris":["`+redirect+`"],"client_name":"Test client","token_endpoint_auth_method":"none"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp, m
	}
	if resp, _ := reg("https://evil.example.com/cb"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("foreign redirect URI registered: HTTP %d", resp.StatusCode)
	}
	resp, client := reg("http://127.0.0.1/callback")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("registration: HTTP %d %v", resp.StatusCode, client)
	}
	clientID := client["client_id"].(string)

	// loopback redirects match on any port
	tok := h.signIn(clientID, "http://127.0.0.1:43123/callback")

	session, err := h.connect(tok["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); got != "get_backlinks,get_page,get_page_history,get_recent_changes,list_pages,search,wiki_info" {
		t.Errorf("tools = %s", got)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "bosh"}})
	if err != nil || res.IsError || !strings.Contains(text(t, res), `"title":"Deploy as jdoe"`) {
		t.Errorf("search = %v %v", text(t, res), err)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_page", Arguments: map[string]any{"id": "deploy", "max_chars": 1000, "offset": 95}})
	if err != nil || res.IsError || len(res.Content) != 2 || res.Content[1].(*mcp.TextContent).Text != "fghij" ||
		!strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"content_chars":100`) {
		t.Errorf("get_page = %v %v", text(t, res), err)
	}
	res, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_page", Arguments: map[string]any{"id": "missing"}})
	if !res.IsError || !strings.Contains(text(t, res), "not found") {
		t.Errorf("missing page = %v", text(t, res))
	}
	res, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "wiki_info", Arguments: map[string]any{}})
	if !strings.Contains(text(t, res), `"id":"XWiki.jdoe"`) {
		t.Errorf("wiki_info = %v", text(t, res))
	}
	session.Close()

	// refresh rotates the refresh token; replaying the old one revokes the session
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {clientID}}
	resp, tok2 := h.postForm("/token", refresh)
	if resp.StatusCode != http.StatusOK || tok2["refresh_token"] == tok["refresh_token"] {
		t.Fatalf("refresh: HTTP %d %v", resp.StatusCode, tok2)
	}
	if resp, body := h.postForm("/token", refresh); resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("replayed refresh token: HTTP %d %v", resp.StatusCode, body)
	}
	if _, err := h.connect(tok2["access_token"].(string)); err == nil {
		t.Error("the access token of a revoked session still works")
	}

	// a password changed in the wiki signs the user out at the next tool call
	tok3 := h.signIn(clientID, "http://127.0.0.1:43123/callback")
	session, err = h.connect(tok3["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	h.engine.passwordChanged.Store(true)
	res, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "bosh"}})
	if !res.IsError || !strings.Contains(text(t, res), "reconnect") {
		t.Errorf("stale credentials: %v", text(t, res))
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "bosh"}}); err == nil {
		t.Error("the session still works after the wiki rejected the credentials")
	}
	refresh.Set("refresh_token", tok3["refresh_token"].(string))
	if resp, body := h.postForm("/token", refresh); body["error"] != "invalid_grant" {
		t.Errorf("refresh after sign-out: HTTP %d %v", resp.StatusCode, body)
	}
}
