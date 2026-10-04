package xwiki

import (
	"errors"
	"net/url"
	"strings"
)

const webHome = "WebHome"

// Reference is an XWiki document reference: wiki, nested spaces and the page name.
type Reference struct {
	Wiki   string
	Spaces []string
	Name   string
}

// ParseReference parses "[wiki:]Space.Sub.Page" with the backslash escapes of XWiki references ("A\.B" is one
// segment). A single segment "Foo" is the nested page Foo.WebHome; the wiki defaults to defaultWiki.
func ParseReference(s, defaultWiki string) (Reference, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Reference{}, errors.New("empty page ID")
	}
	ref := Reference{Wiki: defaultWiki}
	var segments []string
	var cur strings.Builder
	escaped, wikiDone := false, false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == ':' && !wikiDone && len(segments) == 0:
			ref.Wiki = cur.String()
			cur.Reset()
			wikiDone = true
		case r == '.':
			segments = append(segments, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	segments = append(segments, cur.String())
	if ref.Wiki == "" {
		return Reference{}, errors.New("empty wiki name in page ID " + s)
	}
	for _, seg := range segments {
		if seg == "" {
			return Reference{}, errors.New("empty space or page name in page ID " + s)
		}
	}
	if len(segments) == 1 {
		return Reference{Wiki: ref.Wiki, Spaces: segments, Name: webHome}, nil
	}
	ref.Spaces = segments[:len(segments)-1]
	ref.Name = segments[len(segments)-1]
	return ref, nil
}

// referenceFromParts builds a reference from a wiki name and a local space reference ("A.B\.C") as returned by the
// REST API.
func referenceFromParts(wiki, space, name string) Reference {
	ref, err := ParseReference(space+"."+escape(name), wiki)
	if err != nil {
		return Reference{Wiki: wiki, Spaces: []string{space}, Name: name}
	}
	return ref
}

// Local is the reference without the wiki: "Space.Sub.Page".
func (r Reference) Local() string {
	parts := make([]string, 0, len(r.Spaces)+1)
	for _, s := range r.Spaces {
		parts = append(parts, escape(s))
	}
	return strings.Join(append(parts, escape(r.Name)), ".")
}

// Space is the local reference of the space holding the page: "Space.Sub".
func (r Reference) Space() string {
	parts := make([]string, len(r.Spaces))
	for i, s := range r.Spaces {
		parts[i] = escape(s)
	}
	return strings.Join(parts, ".")
}

// ID is the page ID given to the model: the local reference, prefixed with the wiki when it is not defaultWiki.
func (r Reference) ID(defaultWiki string) string {
	if r.Wiki == defaultWiki {
		return r.Local()
	}
	return escapeWiki(r.Wiki) + ":" + r.Local()
}

// Absolute is the reference with the wiki: "xwiki:Space.Page".
func (r Reference) Absolute() string {
	return escapeWiki(r.Wiki) + ":" + r.Local()
}

// Nested returns the nested page form of a terminal page reference: Space.Page -> Space.Page.WebHome.
func (r Reference) Nested() (Reference, bool) {
	if r.Name == webHome {
		return r, false
	}
	spaces := append(append([]string{}, r.Spaces...), r.Name)
	return Reference{Wiki: r.Wiki, Spaces: spaces, Name: webHome}, true
}

// SubtreeSpace is the local space reference of the pages below the page: the page's own space for a nested page
// (A.B.WebHome -> A.B), the page name appended for a terminal page (A.B -> A.B).
func (r Reference) SubtreeSpace() string {
	if n, ok := r.Nested(); ok {
		return n.Space()
	}
	return r.Space()
}

// restPath is the path of the page in the REST API, below /rest.
func (r Reference) restPath() string {
	var b strings.Builder
	b.WriteString("/wikis/")
	b.WriteString(url.PathEscape(r.Wiki))
	for _, s := range r.Spaces {
		b.WriteString("/spaces/")
		b.WriteString(url.PathEscape(s))
	}
	b.WriteString("/pages/")
	b.WriteString(url.PathEscape(r.Name))
	return b.String()
}

// actionPath is the path of the page in the standard XWiki URL scheme, e.g. /bin/view/Space/Sub/ for the nested
// page Space.Sub.WebHome. Pages of other wikis than the main one are given in the path-based subwiki scheme
// (/wiki/<wiki>/view/...), XWiki's domain-based scheme would need the subwiki's own host name.
func (r Reference) actionPath(action, mainWiki string) string {
	var b strings.Builder
	if r.Wiki == mainWiki {
		b.WriteString("/bin/")
	} else {
		b.WriteString("/wiki/")
		b.WriteString(url.PathEscape(r.Wiki))
		b.WriteString("/")
	}
	b.WriteString(action)
	for _, s := range r.Spaces {
		b.WriteString("/")
		b.WriteString(url.PathEscape(s))
	}
	b.WriteString("/")
	if r.Name != webHome {
		b.WriteString(url.PathEscape(r.Name))
	}
	return b.String()
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `.`, `\.`, `:`, `\:`).Replace(s)
}

func escapeWiki(s string) string {
	return strings.NewReplacer(`\`, `\\`, `:`, `\:`, `.`, `\.`).Replace(s)
}
