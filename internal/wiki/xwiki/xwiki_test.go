package xwiki

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

// fakeXWiki imitates the REST API of XWiki 17 for the user jdoe/secret.
func fakeXWiki(t *testing.T) *httptest.Server {
	t.Helper()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// like XWiki 17.10: REST responses have the version, the user only when authenticated
		path := r.URL.EscapedPath()
		if strings.HasPrefix(path, "/rest/") {
			w.Header().Set("XWiki-Version", "17.10.13")
			if user, pass, _ := r.BasicAuth(); user == "jdoe" && pass == "secret" {
				w.Header().Set("XWiki-User", "xwiki:XWiki.jdoe")
			}
		}
		switch {
		case path == "/rest/wikis/xwiki":
			writeJSON(w, map[string]any{"id": "xwiki", "name": "xwiki"})
		case path == "/rest/wikis/xwiki/spaces/XWiki/pages/jdoe/objects/XWiki.XWikiUsers/0":
			writeJSON(w, map[string]any{"className": "XWiki.XWikiUsers", "properties": []map[string]string{
				{"name": "first_name", "value": "John"}, {"name": "last_name", "value": "Doe"}, {"name": "password", "type": "Password", "value": "hash:x"},
			}})
		case path == "/rest/wikis/xwiki/query":
			q := r.URL.Query()
			if q.Get("type") != "solr" {
				http.Error(w, "type", http.StatusBadRequest)
				return
			}
			if strings.Contains(q.Get("q"), "broken[") {
				http.Error(w, "<html><body>org.apache.solr.search.SyntaxError: Cannot parse</body></html>", http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"searchResults": []map[string]any{
				{"type": "page", "pageFullName": "Dev.Deploy.WebHome", "wiki": "xwiki", "space": "Dev.Deploy", "pageName": "WebHome",
					"title": "Deployment", "score": 2.5, "modified": 1759478400000, "author": "xwiki:XWiki.jdoe", "authorName": "John Doe",
					"echo_q": q.Get("q")},
				{"type": "page", "pageFullName": "Dev.Deploy.WebHome", "wiki": "xwiki", "language": "pl", "title": "Wdrożenie"},
				{"type": "page", "pageFullName": `Dev.Release\.Notes`, "wiki": "xwiki", "space": "Dev", "pageName": "Release.Notes"},
			}})
		case path == "/rest/wikis/xwiki/spaces/Dev/pages/Deploy":
			http.Error(w, "not found", http.StatusNotFound)
		case path == "/rest/wikis/xwiki/spaces/Dev/spaces/Deploy/pages/WebHome" && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{
				"id": "xwiki:Dev.Deploy.WebHome", "fullName": "Dev.Deploy.WebHome", "wiki": "xwiki", "space": "Dev.Deploy",
				"name": "WebHome", "title": "Deployment", "version": "3.1", "syntax": "xwiki/2.1", "modified": 1759478400000,
				"modifier": "xwiki:XWiki.jdoe", "modifierName": "John Doe", "content": "= Deploy =\n\nRun **bosh deploy**.",
			})
		case path == "/rest/wikis/xwiki/spaces/Secret/pages/WebHome":
			http.Error(w, "", http.StatusUnauthorized)
		case path == "/bin/get/Dev/Deploy/":
			if r.URL.Query().Get("outputSyntax") != "plain" {
				http.Error(w, "syntax", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, "Deploy\n\nRun bosh deploy.\n")
		default:
			http.Error(w, "unexpected "+r.Method+" "+path, http.StatusTeapot)
		}
	})
	return httptest.NewServer(handler)
}

func newTestEngine(t *testing.T, url string) *Engine {
	t.Helper()
	e, err := New(Config{URL: url, PublicURL: "https://wiki.example.com", Name: "Test XWiki"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestLogin(t *testing.T) {
	srv := fakeXWiki(t)
	defer srv.Close()
	e := newTestEngine(t, srv.URL)

	id, err := e.Login(context.Background(), wiki.Credential{Username: "jdoe", Secret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if id.ID != "xwiki:XWiki.jdoe" || id.DisplayName != "John Doe" {
		t.Errorf("identity = %+v", id)
	}
	_, err = e.Login(context.Background(), wiki.Credential{Username: "jdoe", Secret: "wrong"})
	if !errors.Is(err, wiki.ErrInvalidCredentials) {
		t.Errorf("wrong password: err = %v", err)
	}
}

func TestSessionReads(t *testing.T) {
	srv := fakeXWiki(t)
	defer srv.Close()
	s := newTestEngine(t, srv.URL).Session(wiki.Credential{Username: "jdoe", Secret: "secret"})
	ctx := context.Background()

	res, err := s.Search(ctx, wiki.SearchRequest{Query: "deploy", Under: "Dev", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("hits = %+v, want 2 (the translation folded)", res.Hits)
	}
	h := res.Hits[0]
	if h.ID != "Dev.Deploy.WebHome" || h.URL != "https://wiki.example.com/bin/view/Dev/Deploy/" || h.Author != "John Doe" ||
		!h.Modified.Equal(time.Date(2025, 10, 3, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("hit = %+v", h)
	}
	if res.Hits[1].ID != `Dev.Release\.Notes` || res.Hits[1].Title != "Release.Notes" {
		t.Errorf("hit 2 = %+v", res.Hits[1])
	}

	if _, err := s.Search(ctx, wiki.SearchRequest{Query: "broken[", Limit: 10}); !errors.Is(err, wiki.ErrBadRequest) {
		t.Errorf("invalid Solr query: err = %v", err)
	}

	// Dev.Deploy is not a terminal page, the nested page Dev.Deploy.WebHome is found instead
	p, err := s.GetPage(ctx, wiki.PageRequest{ID: "Dev.Deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "Dev.Deploy.WebHome" || p.Version != "3.1" || p.Markup != "xwiki/2.1" || !strings.Contains(p.Content, "**bosh deploy**") {
		t.Errorf("page = %+v", p)
	}
	p, err = s.GetPage(ctx, wiki.PageRequest{ID: "Dev.Deploy.WebHome", Format: wiki.FormatText})
	if err != nil {
		t.Fatal(err)
	}
	if p.Content != "Deploy\n\nRun bosh deploy." || p.Format != wiki.FormatText {
		t.Errorf("rendered = %q", p.Content)
	}

	if _, err := s.GetPage(ctx, wiki.PageRequest{ID: "Secret"}); !errors.Is(err, wiki.ErrForbidden) {
		t.Errorf("page without rights: err = %v", err)
	}

	guest := newTestEngine(t, srv.URL).Session(wiki.Credential{Username: "jdoe", Secret: "changed"})
	if _, err := guest.GetPage(ctx, wiki.PageRequest{ID: "Dev.Deploy"}); !errors.Is(err, wiki.ErrInvalidCredentials) {
		t.Errorf("stale password: err = %v", err)
	}
}
