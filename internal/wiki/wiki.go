// Package wiki is the engine-independent model of a wiki: the interfaces the MCP tools call and the engine packages
// (xwiki, dokuwiki) implement.
//
// An Engine is one configured wiki shared by all users. Every call made on behalf of a user goes through a Session
// bound to that user's credentials, so the wiki applies its own access rights. Engines add optional capabilities
// (attachments, engine-specific tools) by implementing the extra interfaces below on their Session or
// Engine type.
package wiki

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Errors returned by engines, wrapped with details (errors.Is works).
var (
	// ErrInvalidCredentials means the wiki rejects the user's credentials (wrong or changed password, disabled
	// account).
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrForbidden means the user is authenticated but has no right to do this.
	ErrForbidden = errors.New("access denied")
	ErrNotFound  = errors.New("not found")
	// ErrBadRequest means the wiki rejected the request itself, e.g. a query with a syntax error.
	ErrBadRequest = errors.New("bad request")
)

// Engine is one configured wiki.
type Engine interface {
	Info() Info
	// Login checks the credentials against the wiki and returns the identity of the user.
	// It returns an error wrapping ErrInvalidCredentials when the wiki rejects them.
	Login(ctx context.Context, cred Credential) (*Identity, error)
	// Session returns the wiki API acting as the owner of cred. It is cheap, a session is made per tool call.
	Session(cred Credential) Session
}

// Credential is what the user typed on the sign-in page: a user name and a password (or an API token where the
// engine accepts one instead of the password).
type Credential struct {
	Username string `json:"u"`
	Secret   string `json:"s"`
}

// Identity is the wiki account behind a credential.
type Identity struct {
	// ID is the wiki's identifier of the user, e.g. "xwiki:XWiki.jdoe" or "jdoe".
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"name,omitempty"`
}

// Info describes the wiki to the user and to the model; the help texts go into the tool descriptions.
type Info struct {
	Engine string // "xwiki", "dokuwiki"
	Name   string // display name from the configuration
	URL    string // base URL of the links given to users
	// Markup is the markup language of the page sources, e.g. "xwiki/2.1".
	Markup string
	// PageIDHelp explains how page IDs look.
	PageIDHelp string
	// SearchHelp explains the query syntax of Session.Search.
	SearchHelp string
	// AttachmentsHelp says what the attachments of a page are.
	AttachmentsHelp string
}

// Session is the wiki API of one user.
type Session interface {
	Search(ctx context.Context, req SearchRequest) (*SearchResults, error)
	GetPage(ctx context.Context, req PageRequest) (*Page, error)
	ListPages(ctx context.Context, req ListRequest) (*PageList, error)
	History(ctx context.Context, req HistoryRequest) (*History, error)
	RecentChanges(ctx context.Context, req ChangesRequest) (*Changes, error)
	// Backlinks lists the pages linking to the page.
	Backlinks(ctx context.Context, id string, limit int) ([]PageItem, error)
}

// AttachmentReader is implemented by sessions that can list and read files attached to pages.
type AttachmentReader interface {
	Attachments(ctx context.Context, pageID string) ([]Attachment, error)
	// Attachment reads at most maxBytes of the file; Truncated is set when the file is larger.
	Attachment(ctx context.Context, pageID, name string, maxBytes int64) (*AttachmentData, error)
}

// ToolProvider is implemented by engines with tools of their own. Their names start with the engine name
// ("<engine>_<tool>"); a handler gets the user's session from tools.Session, type-asserts it to the engine's type
// and passes its errors through tools.Error.
type ToolProvider interface {
	AddTools(server *mcp.Server, tools Tools)
}

// Tools is what the MCP server gives the tools of an engine.
type Tools struct {
	// Session returns the session of the user calling the tool.
	Session func(ctx context.Context, req *mcp.CallToolRequest) (Session, error)
	// Error turns an engine error into the error reported to the model; on ErrInvalidCredentials it also signs
	// the user out, so that the client asks them to sign in again.
	Error func(ctx context.Context, req *mcp.CallToolRequest, err error) error
}

// Sort orders search results.
type Sort string

const (
	SortRelevance Sort = "relevance"
	SortNewest    Sort = "newest"
)

// Format of page content.
type Format string

const (
	// FormatSource is the wiki markup as stored.
	FormatSource Format = "source"
	// FormatText is the page rendered by the wiki (macros and includes expanded) as plain text.
	FormatText Format = "text"
	// FormatHTML is the page rendered by the wiki as an HTML fragment.
	FormatHTML Format = "html"
)

type SearchRequest struct {
	Query string
	// Under limits the search to the pages below this page or namespace.
	Under  string
	Sort   Sort
	Limit  int
	Offset int
}

type SearchResults struct {
	Hits    []SearchHit `json:"hits"`
	Offset  int         `json:"offset"`
	HasMore bool        `json:"has_more"`
	// Total is the number of all hits when the engine knows it.
	Total int `json:"total,omitempty"`
}

type SearchHit struct {
	ID       string    `json:"id"`
	Title    string    `json:"title,omitempty"`
	URL      string    `json:"url,omitempty"`
	Location string    `json:"location,omitempty"`
	Snippet  string    `json:"snippet,omitempty"`
	Score    float64   `json:"score,omitempty"`
	Modified time.Time `json:"modified,omitzero"`
	Author   string    `json:"author,omitempty"`
}

type PageRequest struct {
	ID string
	// Version is an older version of the page, "" for the current one.
	Version string
	Format  Format
}

type Page struct {
	ID       string    `json:"id"`
	Title    string    `json:"title,omitempty"`
	URL      string    `json:"url,omitempty"`
	Location string    `json:"location,omitempty"`
	Version  string    `json:"version,omitempty"`
	Modified time.Time `json:"modified,omitzero"`
	Author   string    `json:"author,omitempty"`
	// Markup is the markup language of the source, set also when Content is rendered.
	Markup  string `json:"markup,omitempty"`
	Format  Format `json:"format"`
	Content string `json:"-"`
}

type ListRequest struct {
	// Parent is a page or namespace ID, "" for the top level.
	Parent string
	// Recursive lists all pages below Parent instead of its direct children.
	Recursive bool
	Limit     int
	Offset    int
}

type PageList struct {
	Items   []PageItem `json:"items"`
	Offset  int        `json:"offset"`
	HasMore bool       `json:"has_more"`
}

// Item kinds.
const (
	KindPage      = "page"
	KindNamespace = "namespace"
)

type PageItem struct {
	ID       string    `json:"id"`
	Title    string    `json:"title,omitempty"`
	URL      string    `json:"url,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Modified time.Time `json:"modified,omitzero"`
}

type HistoryRequest struct {
	ID     string
	Limit  int
	Offset int
}

type History struct {
	ID        string     `json:"id"`
	Revisions []Revision `json:"revisions"`
	Offset    int        `json:"offset"`
	HasMore   bool       `json:"has_more"`
}

type Revision struct {
	Version  string    `json:"version"`
	Modified time.Time `json:"modified,omitzero"`
	Author   string    `json:"author,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	Type     string    `json:"type,omitempty"`
}

type ChangesRequest struct {
	// Since limits the changes to the newer ones, zero for the engine's default window.
	Since  time.Time
	Limit  int
	Offset int
}

type Changes struct {
	Changes []Change `json:"changes"`
	Offset  int      `json:"offset"`
	HasMore bool     `json:"has_more"`
}

type Change struct {
	ID       string    `json:"id"`
	URL      string    `json:"url,omitempty"`
	Version  string    `json:"version,omitempty"`
	Modified time.Time `json:"modified,omitzero"`
	Author   string    `json:"author,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	Type     string    `json:"type,omitempty"`
}

type Attachment struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	MimeType string    `json:"mime_type,omitempty"`
	Modified time.Time `json:"modified,omitzero"`
	Author   string    `json:"author,omitempty"`
	Version  string    `json:"version,omitempty"`
	URL      string    `json:"url,omitempty"`
}

type AttachmentData struct {
	Attachment
	Data      []byte
	Truncated bool
}
