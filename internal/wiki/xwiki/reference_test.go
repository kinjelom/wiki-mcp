package xwiki

import (
	"reflect"
	"testing"
)

func TestParseReference(t *testing.T) {
	tests := []struct {
		in   string
		want Reference
		id   string
	}{
		{"Main.WebHome", Reference{"xwiki", []string{"Main"}, "WebHome"}, "Main.WebHome"},
		{"A.B.C", Reference{"xwiki", []string{"A", "B"}, "C"}, "A.B.C"},
		{"Sandbox", Reference{"xwiki", []string{"Sandbox"}, "WebHome"}, "Sandbox.WebHome"},
		{"sub:Space.Page", Reference{"sub", []string{"Space"}, "Page"}, "sub:Space.Page"},
		{"xwiki:Space.Page", Reference{"xwiki", []string{"Space"}, "Page"}, "Space.Page"},
		{`Release\.Notes.v1\.2`, Reference{"xwiki", []string{"Release.Notes"}, "v1.2"}, `Release\.Notes.v1\.2`},
		{`A.Time\: 12\:00`, Reference{"xwiki", []string{"A"}, "Time: 12:00"}, `A.Time\: 12\:00`},
		{`A.Back\\slash`, Reference{"xwiki", []string{"A"}, `Back\slash`}, `A.Back\\slash`},
		{"  Zażółć.Gęślą jaźń ", Reference{"xwiki", []string{"Zażółć"}, "Gęślą jaźń"}, "Zażółć.Gęślą jaźń"},
	}
	for _, tt := range tests {
		got, err := ParseReference(tt.in, "xwiki")
		if err != nil {
			t.Fatalf("ParseReference(%q): %v", tt.in, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseReference(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
		if id := got.ID("xwiki"); id != tt.id {
			t.Errorf("ParseReference(%q).ID() = %q, want %q", tt.in, id, tt.id)
		}
	}
	for _, bad := range []string{"", "A..B", ":A.B", "A.", ".A"} {
		if _, err := ParseReference(bad, "xwiki"); err == nil {
			t.Errorf("ParseReference(%q) succeeded", bad)
		}
	}
}

func TestReferencePaths(t *testing.T) {
	ref := Reference{"xwiki", []string{"Dev Ops", "a/b"}, "Page 1"}
	if got, want := ref.restPath(), "/wikis/xwiki/spaces/Dev%20Ops/spaces/a%2Fb/pages/Page%201"; got != want {
		t.Errorf("restPath = %q, want %q", got, want)
	}
	if got, want := ref.actionPath("view", "xwiki"), "/bin/view/Dev%20Ops/a%2Fb/Page%201"; got != want {
		t.Errorf("actionPath = %q, want %q", got, want)
	}
	nested := Reference{"sub", []string{"A", "B"}, "WebHome"}
	if got, want := nested.actionPath("get", "xwiki"), "/wiki/sub/get/A/B/"; got != want {
		t.Errorf("actionPath = %q, want %q", got, want)
	}

	terminal := Reference{"xwiki", []string{"A"}, "B"}
	n, ok := terminal.Nested()
	if !ok || n.Local() != "A.B.WebHome" {
		t.Errorf("Nested() = %v %v", n, ok)
	}
	if got := terminal.SubtreeSpace(); got != "A.B" {
		t.Errorf("SubtreeSpace() = %q", got)
	}
	if got := n.SubtreeSpace(); got != "A.B" {
		t.Errorf("SubtreeSpace() of nested = %q", got)
	}
	if _, ok := n.Nested(); ok {
		t.Error("Nested() of a WebHome reference must be false")
	}
}
