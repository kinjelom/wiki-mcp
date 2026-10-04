package wiki

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTMLToText turns an HTML fragment rendered by a wiki into readable plain text: headings as "#", list items as
// "-", table cells separated by "|", preformatted blocks kept as they are, everything else as paragraphs.
func HTMLToText(fragment string) string {
	nodes, err := html.ParseFragment(strings.NewReader(fragment), &html.Node{Type: html.ElementNode, DataAtom: atom.Body, Data: "body"})
	if err != nil {
		return fragment
	}
	t := &textWriter{}
	for _, n := range nodes {
		t.node(n)
	}
	return t.String()
}

type textWriter struct {
	b   strings.Builder
	pre int
	// space is a pending separator between inline words
	space bool
}

func (t *textWriter) node(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		t.text(n.Data)
		return
	case html.ElementNode:
	default:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			t.node(c)
		}
		return
	}

	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Head, atom.Noscript, atom.Template:
		return
	case atom.Br:
		t.newline()
		return
	case atom.Hr:
		t.block()
		t.write("---")
		t.block()
		return
	case atom.Img:
		if alt := attr(n, "alt"); alt != "" {
			t.text("[" + alt + "]")
		}
		return
	}

	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		t.block()
		t.write(strings.Repeat("#", int(n.Data[1]-'0')) + " ")
	case atom.Li:
		t.newline()
		t.write("- ")
	case atom.Tr:
		t.newline()
	case atom.Td, atom.Th:
		if n.PrevSibling != nil {
			t.write(" | ")
		}
	case atom.Pre:
		t.block()
		t.pre++
		defer func() { t.pre--; t.block() }()
	case atom.P, atom.Div, atom.Section, atom.Article, atom.Table, atom.Ul, atom.Ol, atom.Dl, atom.Blockquote,
		atom.Figure, atom.Header, atom.Footer, atom.Nav, atom.Aside, atom.Dt, atom.Dd:
		t.block()
		defer t.block()
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		t.node(c)
	}
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		t.block()
	}
}

func (t *textWriter) text(s string) {
	if t.pre > 0 {
		t.write(s)
		return
	}
	for i, field := range strings.FieldsFunc(s, unicode.IsSpace) {
		if i > 0 || (len(s) > 0 && unicode.IsSpace(rune(s[0]))) {
			t.space = true
		}
		if t.space && !t.atLineStart() {
			t.b.WriteByte(' ')
		}
		t.space = false
		t.b.WriteString(field)
	}
	if len(s) > 0 && unicode.IsSpace(rune(s[len(s)-1])) {
		t.space = true
	}
}

func (t *textWriter) write(s string) {
	t.space = false
	t.b.WriteString(s)
}

func (t *textWriter) atLineStart() bool {
	s := t.b.String()
	return s == "" || strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "- ") || strings.HasSuffix(s, "# ")
}

func (t *textWriter) newline() {
	t.space = false
	if s := t.b.String(); s != "" && !strings.HasSuffix(s, "\n") {
		t.b.WriteByte('\n')
	}
}

// block ends the current line and leaves one empty line before the next block.
func (t *textWriter) block() {
	t.space = false
	s := t.b.String()
	switch {
	case s == "" || strings.HasSuffix(s, "\n\n"):
	case strings.HasSuffix(s, "\n"):
		t.b.WriteByte('\n')
	default:
		t.b.WriteString("\n\n")
	}
}

func (t *textWriter) String() string {
	lines := strings.Split(t.b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// Window returns the window [offset, offset+limit) of items and whether more items follow.
func Window[T any](items []T, offset, limit int) ([]T, bool) {
	if offset >= len(items) {
		return []T{}, false
	}
	items = items[offset:]
	if limit > 0 && len(items) > limit {
		return items[:limit], true
	}
	return items, false
}
