package dokuwiki

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

const testToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJqZG9lIn0.c2lnbmF0dXJl"

// fakeDokuWiki imitates the JSON-RPC API of DokuWiki for jdoe/secret and jdoe's token.
func fakeDokuWiki(t *testing.T) *httptest.Server {
	t.Helper()
	pages := []map[string]any{
		{"id": "start", "revision": 1700000000, "title": "Home"},
		{"id": "ops:start", "revision": 1700000100, "title": "Ops"},
		{"id": "ops:bosh:deploy", "revision": 1700000200, "title": "Deploy"},
		{"id": "ops:bosh:stemcells", "revision": 1700000300, "title": "Stemcells"},
		{"id": "dev:go", "revision": 1700000400, "title": "Go"},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lib/exe/jsonrpc.php" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		user, pass, basic := r.BasicAuth()
		authed := (basic && user == "jdoe" && pass == "secret") || r.Header.Get("Authorization") == "Bearer "+testToken
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := func(status int, result any, code int, msg string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			resp := map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}
			if code != 0 {
				resp["error"] = map[string]any{"code": code, "message": msg}
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
		if !authed {
			reply(http.StatusUnauthorized, nil, -32603, "server error. not authorized to call method "+req.Method)
			return
		}
		p := req.Params
		switch req.Method {
		case "core.whoAmI":
			reply(200, map[string]any{"login": "jdoe", "name": "John Doe", "groups": []string{"user"}}, 0, "")
		case "core.searchPages":
			if p["query"] != "bosh @ops" {
				reply(400, nil, 1, "unexpected query "+p["query"].(string))
				return
			}
			reply(200, []map[string]any{
				{"id": "ops:bosh:deploy", "score": 3, "revision": 1700000200, "title": "Deploy", "snippet": `How to <strong class="search_hit">bosh</strong> deploy`},
				{"id": "ops:bosh:stemcells", "score": 1, "revision": 1700000300, "title": "Stemcells"},
			}, 0, "")
		case "core.getPageInfo":
			for _, pg := range pages {
				if pg["id"] == p["page"] {
					reply(200, pg, 0, "")
					return
				}
			}
			if p["page"] == "secret" {
				reply(403, nil, 111, "You are not allowed to read this page")
				return
			}
			reply(400, nil, 121, "The requested page (revision) does not exist")
		case "core.getPage":
			reply(200, "====== Deploy ======\n  * bosh deploy\n", 0, "")
		case "core.getPageHTML":
			reply(200, "<h1>Deploy</h1><ul><li>bosh deploy</li></ul>", 0, "")
		case "core.listPages":
			reply(200, pages, 0, "")
		case "core.getRecentPageChanges":
			reply(200, []map[string]any{
				{"id": "ops:bosh:deploy", "revision": 1700000200, "author": "jdoe", "summary": "fix", "type": "E", "ip": "10.0.0.1"},
				{"id": "dev:go", "revision": 1700000400, "author": "jdoe", "type": "C"},
			}, 0, "")
		case "core.getMediaInfo":
			reply(200, map[string]any{"id": "ops:bosh:notes.txt", "size": 5, "revision": 1700000500}, 0, "")
		case "core.getMedia":
			reply(200, base64.StdEncoding.EncodeToString([]byte("hello")), 0, "")
		default:
			reply(400, nil, -32601, "method not found")
		}
	})
	return httptest.NewServer(handler)
}

func TestDokuWiki(t *testing.T) {
	srv := fakeDokuWiki(t)
	defer srv.Close()
	e, err := New(Config{URL: srv.URL, PublicURL: "https://doku.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	id, err := e.Login(ctx, wiki.Credential{Username: "jdoe", Secret: "secret"})
	if err != nil || id.ID != "jdoe" || id.DisplayName != "John Doe" {
		t.Fatalf("login = %+v, %v", id, err)
	}
	if _, err := e.Login(ctx, wiki.Credential{Username: "anything", Secret: testToken}); err != nil {
		t.Errorf("token login: %v", err)
	}
	if _, err := e.Login(ctx, wiki.Credential{Username: "jdoe", Secret: "wrong"}); !errors.Is(err, wiki.ErrInvalidCredentials) {
		t.Errorf("wrong password: err = %v", err)
	}

	s := e.Session(wiki.Credential{Username: "jdoe", Secret: "secret"})
	res, err := s.Search(ctx, wiki.SearchRequest{Query: "bosh", Under: "ops", Sort: wiki.SortNewest, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].ID != "ops:bosh:stemcells" || !res.HasMore || res.Total != 2 {
		t.Errorf("search = %+v", res)
	}
	res, _ = s.Search(ctx, wiki.SearchRequest{Query: "bosh", Under: "ops", Limit: 5})
	if res.Hits[0].Snippet != "How to bosh deploy" || res.Hits[0].URL != "https://doku.example.com/doku.php?id=ops%3Abosh%3Adeploy" {
		t.Errorf("hit = %+v", res.Hits[0])
	}

	p, err := s.GetPage(ctx, wiki.PageRequest{ID: "Ops:Bosh:Deploy", Format: wiki.FormatText})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "ops:bosh:deploy" || p.Version != "1700000200" || p.Content != "# Deploy\n\n- bosh deploy" {
		t.Errorf("page = %+v %q", p, p.Content)
	}
	if _, err := s.GetPage(ctx, wiki.PageRequest{ID: "missing"}); !errors.Is(err, wiki.ErrNotFound) {
		t.Errorf("missing page: err = %v", err)
	}
	if _, err := s.GetPage(ctx, wiki.PageRequest{ID: "secret"}); !errors.Is(err, wiki.ErrForbidden) {
		t.Errorf("forbidden page: err = %v", err)
	}

	list, err := s.ListPages(ctx, wiki.ListRequest{Parent: "ops", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range list.Items {
		got = append(got, it.Kind+":"+it.ID)
	}
	if strings.Join(got, ",") != "namespace:ops:bosh,page:ops:start" {
		t.Errorf("list = %v", got)
	}
	list, _ = s.ListPages(ctx, wiki.ListRequest{Parent: "ops", Recursive: true, Limit: 10})
	if len(list.Items) != 3 {
		t.Errorf("recursive list = %+v", list.Items)
	}

	ch, err := s.RecentChanges(ctx, wiki.ChangesRequest{Limit: 10})
	if err != nil || len(ch.Changes) != 2 || ch.Changes[0].ID != "dev:go" || ch.Changes[0].Type != "created" {
		t.Errorf("changes = %+v, %v", ch, err)
	}

	att, err := s.(wiki.AttachmentReader).Attachment(ctx, "ops:bosh:deploy", "notes.txt", 1024)
	if err != nil || string(att.Data) != "hello" || att.MimeType != "text/plain; charset=utf-8" {
		t.Errorf("attachment = %+v, %v", att, err)
	}
}
