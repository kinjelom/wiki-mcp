//go:build live

// Tests against a running XWiki 16.4+. The pages they read are created under the space WikiMcpTest with the REST API
// directly, wiki-mcp itself only reads:
//
//	XWIKI_URL=http://127.0.0.1:8080 XWIKI_USER=Admin XWIKI_PASSWORD=... go test -tags live -run Live ./internal/wiki/xwiki/
package xwiki

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

func TestLiveXWiki(t *testing.T) {
	base, user, pass := os.Getenv("XWIKI_URL"), os.Getenv("XWIKI_USER"), os.Getenv("XWIKI_PASSWORD")
	if base == "" {
		t.Skip("XWIKI_URL is not set")
	}
	e, err := New(Config{URL: base, Name: "live"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cred := wiki.Credential{Username: user, Secret: pass}

	id, err := e.Login(ctx, cred)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Logf("signed in as %+v", id)
	if _, err := e.Login(ctx, wiki.Credential{Username: user, Secret: pass + "-wrong"}); !errors.Is(err, wiki.ErrInvalidCredentials) {
		t.Errorf("wrong password: err = %v", err)
	}

	s := e.Session(cred).(*Session)
	stamp := time.Now().Format("150405")
	parent, child := "WikiMcpTest.Run"+stamp, "WikiMcpTest.Run"+stamp+".Child"

	putPage(t, e, user, pass, parent+".WebHome", "Run "+stamp, "wiki-mcp live test",
		"= Deployment notes =\n\nWe deploy with **bosh** zażółć"+stamp+".\n\nSee [[Child>>"+child+".WebHome]].")
	first, err := s.GetPage(ctx, wiki.PageRequest{ID: parent})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	putPage(t, e, user, pass, child+".WebHome", "", "", "Child page about stemcells.")
	putPage(t, e, user, pass, parent+".WebHome", "", "",
		"= Deployment notes =\n\nUpdated **bosh** zażółć"+stamp+".\n\nSee [[Child>>"+child+".WebHome]].")

	p, err := s.GetPage(ctx, wiki.PageRequest{ID: parent})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	t.Logf("page %s v%s by %s at %s, %s", p.ID, p.Version, p.Author, p.Modified, p.URL)
	if p.Title != "Run "+stamp || p.Markup != "xwiki/2.1" || !strings.Contains(p.Content, "Updated **bosh**") || p.Modified.IsZero() {
		t.Errorf("page = %+v %q", p, p.Content)
	}
	txt, err := s.GetPage(ctx, wiki.PageRequest{ID: parent, Format: wiki.FormatText})
	if err != nil || !strings.Contains(txt.Content, "Updated bosh") || strings.Contains(txt.Content, "**") {
		t.Errorf("text = %q %v", txt.Content, err)
	}
	html, err := s.GetPage(ctx, wiki.PageRequest{ID: parent, Format: wiki.FormatHTML})
	if err != nil || !strings.Contains(html.Content, "<strong>bosh</strong>") {
		t.Errorf("html = %q %v", html.Content, err)
	}
	old, err := s.GetPage(ctx, wiki.PageRequest{ID: parent, Version: first.Version})
	if err != nil || !strings.Contains(old.Content, "We deploy") {
		t.Errorf("old version = %q %v", old.Content, err)
	}

	list, err := s.ListPages(ctx, wiki.ListRequest{Parent: parent, Limit: 10})
	if err != nil || len(list.Items) != 1 || list.Items[0].ID != child+".WebHome" {
		t.Errorf("children = %+v %v", list, err)
	}
	// WikiMcpTest has no home page: the children come from the Solr index (after it indexed the pages)
	time.Sleep(3 * time.Second)
	top, err := s.ListPages(ctx, wiki.ListRequest{Limit: 100})
	if err != nil || !hasItem(top.Items, "WikiMcpTest.WebHome") {
		t.Errorf("top level = %+v %v", top, err)
	}
	space, err := s.ListPages(ctx, wiki.ListRequest{Parent: "WikiMcpTest", Limit: 100})
	if err != nil || !hasItem(space.Items, parent+".WebHome") || hasItem(space.Items, child+".WebHome") {
		t.Errorf("children of a space without home page = %+v %v", space, err)
	}

	h, err := s.History(ctx, wiki.HistoryRequest{ID: parent, Limit: 5})
	if err != nil || len(h.Revisions) != 2 || h.Revisions[1].Summary != "wiki-mcp live test" {
		t.Errorf("history = %+v %v", h, err)
	}
	ch, err := s.RecentChanges(ctx, wiki.ChangesRequest{Since: time.Now().Add(-time.Hour), Limit: 10})
	if err != nil || len(ch.Changes) == 0 {
		t.Errorf("changes = %+v %v", ch, err)
	} else {
		t.Logf("latest change %+v", ch.Changes[0])
	}

	// attachment through the REST API, then the attachment tools
	req, _ := http.NewRequest(http.MethodPut, e.restURL(mustRef(t, parent+".WebHome").restPath()+"/attachments/notes.txt"), strings.NewReader("hello from wiki-mcp"))
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "text/plain")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode >= 300 {
		t.Fatalf("upload: %v %v", resp, err)
	}
	atts, err := s.Attachments(ctx, parent)
	if err != nil || len(atts) != 1 || atts[0].Name != "notes.txt" || atts[0].Size != 19 {
		t.Errorf("attachments = %+v %v", atts, err)
	}
	data, err := s.Attachment(ctx, parent, "notes.txt", 5)
	if err != nil || string(data.Data) != "hello" || !data.Truncated {
		t.Errorf("attachment = %+v %v", data, err)
	}

	// Solr indexes asynchronously
	var hits *wiki.SearchResults
	for i := 0; i < 30; i++ {
		hits, err = s.Search(ctx, wiki.SearchRequest{Query: "zażółć" + stamp, Limit: 5})
		if err == nil && len(hits.Hits) > 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil || len(hits.Hits) == 0 || hits.Hits[0].ID != parent+".WebHome" {
		t.Errorf("search = %+v %v", hits, err)
	} else {
		t.Logf("hit %+v", hits.Hits[0])
	}
	under, err := s.Search(ctx, wiki.SearchRequest{Query: "stemcells", Under: parent, Limit: 5})
	if err != nil || len(under.Hits) != 1 || under.Hits[0].ID != child+".WebHome" {
		t.Errorf("search under = %+v %v", under, err)
	}
	// the extended DisMax parser escapes unbalanced syntax instead of failing
	if _, err := s.Search(ctx, wiki.SearchRequest{Query: "title:(broken", Limit: 5}); err != nil {
		t.Errorf("unbalanced query: err = %v", err)
	}
	newest, err := s.Search(ctx, wiki.SearchRequest{Query: "*:*", Sort: wiki.SortNewest, Limit: 3})
	if err != nil || len(newest.Hits) == 0 {
		t.Errorf("newest = %+v %v", newest, err)
	}
	rec, err := s.ListPages(ctx, wiki.ListRequest{Parent: "WikiMcpTest", Recursive: true, Limit: 50})
	if err != nil || !hasItem(rec.Items, child+".WebHome") {
		t.Errorf("recursive list = %+v %v", rec, err)
	}
	var back []wiki.PageItem
	for i := 0; i < 15; i++ {
		back, err = s.Backlinks(ctx, child, 10)
		if err == nil && len(back) > 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil || len(back) != 1 || back[0].ID != parent+".WebHome" {
		t.Errorf("backlinks = %+v %v", back, err)
	}

	if _, err := s.GetPage(ctx, wiki.PageRequest{ID: "WikiMcpTest.DoesNotExist" + stamp}); !errors.Is(err, wiki.ErrNotFound) {
		t.Errorf("missing page: err = %v", err)
	}
	stale := e.Session(wiki.Credential{Username: user, Secret: "changed"})
	if _, err := stale.GetPage(ctx, wiki.PageRequest{ID: parent}); !errors.Is(err, wiki.ErrInvalidCredentials) {
		t.Errorf("stale password: err = %v", err)
	}
}

// putPage creates or updates a page through the REST API: the fixtures of the test.
func putPage(t *testing.T, e *Engine, user, pass, id, title, comment, content string) {
	t.Helper()
	var b strings.Builder
	element := func(name, value string) {
		if value != "" {
			b.WriteString("<" + name + ">")
			_ = xml.EscapeText(&b, []byte(value))
			b.WriteString("</" + name + ">")
		}
	}
	b.WriteString(`<page xmlns="http://www.xwiki.org">`)
	element("title", title)
	element("syntax", "xwiki/2.1")
	element("comment", comment)
	element("content", content)
	b.WriteString("</page>")
	req, _ := http.NewRequest(http.MethodPut, e.restURL(mustRef(t, id).restPath()), strings.NewReader(b.String()))
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", id, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("PUT %s: HTTP %d", id, resp.StatusCode)
	}
}

func hasItem(items []wiki.PageItem, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func mustRef(t *testing.T, id string) Reference {
	ref, err := ParseReference(id, "xwiki")
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
