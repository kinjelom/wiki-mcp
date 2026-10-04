package oauth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

type okAuth struct{}

func (okAuth) Login(context.Context, wiki.Credential) (*wiki.Identity, error) {
	return &wiki.Identity{ID: "jdoe", Login: "jdoe"}, nil
}

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	store, err := OpenStore(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if cfg.Issuer == "" {
		cfg.Issuer = "https://mcp.example.com"
		cfg.Resource = cfg.Issuer + "/mcp"
	}
	s, err := New(cfg, store, make([]byte, KeySize), okAuth{}, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestClientMetadataDocument follows Claude's way of identifying itself: the client ID is the URL of its metadata
// document.
func TestClientMetadataDocument(t *testing.T) {
	var docURL string
	doc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":                  docURL,
			"client_name":                "Claude",
			"redirect_uris":              []string{"https://claude.ai/api/mcp/auth_callback"},
			"token_endpoint_auth_method": "none",
		})
	}))
	defer doc.Close()
	docURL = doc.URL + "/oauth/mcp-oauth-client-metadata"

	s := newTestServer(t, Config{ClientMetadataHosts: []string{"127.0.0.1"}})
	s.metadataClient = doc.Client()

	authorize := func(clientID, redirect string) *httptest.ResponseRecorder {
		q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
			"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
		rec := httptest.NewRecorder()
		s.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
		return rec
	}
	if rec := authorize(docURL, "https://claude.ai/api/mcp/auth_callback"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "Claude") || !strings.Contains(rec.Body.String(), "claude.ai") {
		t.Fatalf("metadata document client: HTTP %d\n%s", rec.Code, rec.Body)
	}
	if rec := authorize(docURL, "https://claude.ai/other"); rec.Code != http.StatusBadRequest {
		t.Errorf("redirect URI outside the document: HTTP %d", rec.Code)
	}
	// documents are fetched only from the allowed hosts
	s.cfg.ClientMetadataHosts = []string{"claude.ai"}
	s.metadata.entries = map[string]cachedMetadata{}
	if rec := authorize(docURL, "https://claude.ai/api/mcp/auth_callback"); rec.Code != http.StatusBadRequest {
		t.Errorf("document from a host not allowed: HTTP %d", rec.Code)
	}
}

func TestAuthorizeErrorsGoBackToTheClient(t *testing.T) {
	s := newTestServer(t, Config{DynamicRegistration: true, AllowedRedirectURIs: []string{"https://app.example.com/cb"}})
	if err := s.store.PutClient(&Client{ClientID: "c1", RedirectURIs: []string{"https://app.example.com/cb"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"response_type": {"code"}, "client_id": {"c1"}, "redirect_uri": {"https://app.example.com/cb"}, "state": {"s1"}}
	rec := httptest.NewRecorder()
	s.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || loc.Query().Get("error") != "invalid_request" || loc.Query().Get("state") != "s1" {
		t.Errorf("missing PKCE: HTTP %d %s", rec.Code, loc)
	}
}

func TestRedirectMatching(t *testing.T) {
	tests := []struct {
		registered, requested string
		want                  bool
	}{
		{"https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback", true},
		{"https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback/x", false},
		{"http://localhost/callback", "http://localhost:3118/callback", true},
		{"http://127.0.0.1/callback", "http://127.0.0.1:50000/callback", true},
		{"http://127.0.0.1/callback", "http://127.0.0.1:50000/other", false},
		{"http://localhost/callback", "http://evil.example.com:3118/callback", false},
		{"https://app.example.com/cb", "https://app.example.com:8443/cb", false},
	}
	for _, tt := range tests {
		if got := redirectMatches(tt.registered, tt.requested); got != tt.want {
			t.Errorf("redirectMatches(%q, %q) = %v", tt.registered, tt.requested, got)
		}
	}
	if !redirectAllowed("https://x.example.com/anything", []string{"https://x.example.com/*"}) {
		t.Error("prefix pattern does not match")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(3, time.Minute)
	now := time.Now()
	for range 3 {
		l.failure("k", now)
	}
	if !l.blocked("k", now) {
		t.Error("not blocked after 3 failures")
	}
	if l.blocked("k", now.Add(2*time.Minute)) {
		t.Error("still blocked after the window")
	}
}

func TestKeys(t *testing.T) {
	path := t.TempDir() + "/k/encryption.key"
	k1, created, err := LoadOrCreateKey(path)
	if err != nil || !created || len(k1) != KeySize {
		t.Fatalf("create: %v %v", created, err)
	}
	k2, created, err := LoadOrCreateKey(path)
	if err != nil || created || string(k1) != string(k2) {
		t.Fatalf("load: %v %v", created, err)
	}
	if _, err := ParseKey("too short"); err == nil {
		t.Error("short key accepted")
	}
	if k, err := ParseKey("Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4zAb3dEf6hIj9kLm2nOp5qRs8tUv1wXy4z"); err != nil || len(k) != KeySize {
		t.Errorf("passphrase: %v", err)
	}
	s, _ := newSealer(k1)
	sealed := s.seal([]byte("secret"), "a")
	if _, err := s.open(sealed, "b"); err == nil {
		t.Error("a value sealed for one purpose opens for another")
	}
}
