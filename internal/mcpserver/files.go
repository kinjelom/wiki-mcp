package mcpserver

import (
	"context"
	"fmt"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kinjelom/wiki-mcp/internal/oauth"
	"github.com/kinjelom/wiki-mcp/internal/wiki"
)

const (
	defaultAttachmentBytes = 512 << 10
	maxAttachmentBytes     = 5 << 20
)

func (b *builder) addAttachmentTools(server *mcp.Server) {
	addTool(b, server, &mcp.Tool{
		Name:        "list_attachments",
		Title:       "List attachments",
		Description: "Lists the files of a page: " + b.info.AttachmentsHelp,
		Annotations: readOnly,
	}, b.listAttachments)

	addTool(b, server, &mcp.Tool{
		Name:  "get_attachment",
		Title: "Read an attachment",
		Description: "Reads a file of a page (" + b.info.AttachmentsHelp + ") Text files (text/*, JSON, XML, YAML, " +
			"CSV, Markdown, scripts) are returned as text and images as images; other formats (PDF, office " +
			"documents) only as metadata with their URL, their text is found by search where the wiki indexes it.",
		Annotations: readOnly,
		InputSchema: inputSchema[getAttachmentInput](func(p map[string]*jsonschema.Schema) {
			between(p["max_bytes"], 1024, maxAttachmentBytes)
		}),
	}, b.getAttachment)
}

type listAttachmentsInput struct {
	ID string `json:"id" jsonschema:"page ID"`
}

func (b *builder) listAttachments(ctx context.Context, s wiki.Session, _ *oauth.User, in listAttachmentsInput) (*mcp.CallToolResult, error) {
	atts, err := s.(wiki.AttachmentReader).Attachments(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return wiki.JSONResult(map[string]any{"id": in.ID, "attachments": atts})
}

type getAttachmentInput struct {
	ID       string `json:"id" jsonschema:"page ID"`
	Name     string `json:"name" jsonschema:"file name as returned by list_attachments"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes to read, default 524288"`
}

func (b *builder) getAttachment(ctx context.Context, s wiki.Session, _ *oauth.User, in getAttachmentInput) (*mcp.CallToolResult, error) {
	limit := int64(clamp(in.MaxBytes, defaultAttachmentBytes, maxAttachmentBytes))
	att, err := s.(wiki.AttachmentReader).Attachment(ctx, in.ID, in.Name, limit)
	if err != nil {
		return nil, err
	}
	meta := map[string]any{"attachment": att.Attachment, "truncated": att.Truncated}
	mimeType := mediaType(att.MimeType)
	switch {
	case isText(mimeType, att.Name) && utf8.Valid(att.Data):
		head, err := wiki.JSONText(meta)
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: head}, &mcp.TextContent{Text: string(att.Data)}}}, nil
	case isImage(mimeType) && !att.Truncated && len(att.Data) > 0:
		head, err := wiki.JSONText(meta)
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: head}, &mcp.ImageContent{Data: att.Data, MIMEType: mimeType}}}, nil
	}
	if att.Truncated && isImage(mimeType) {
		meta["note"] = fmt.Sprintf("the image is larger than max_bytes (%d), call again with a higher max_bytes", limit)
	} else {
		meta["note"] = fmt.Sprintf("%s content is not returned; the file is available at its url", mimeType)
	}
	return wiki.JSONResult(meta)
}

func mediaType(contentType string) string {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		return mt
	}
	return "application/octet-stream"
}

func isImage(mt string) bool {
	switch mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

var textTypes = []string{
	"application/json", "application/xml", "application/yaml", "application/x-yaml", "application/javascript",
	"application/x-sh", "application/sql", "application/toml", "application/x-httpd-php", "image/svg+xml",
}

var textExtensions = []string{".md", ".txt", ".csv", ".log", ".yml", ".yaml", ".json", ".xml", ".ini", ".conf",
	".properties", ".sh", ".py", ".go", ".java", ".js", ".ts", ".sql", ".html", ".css", ".toml", ".tf", ".hcl"}

func isText(mt, name string) bool {
	if strings.HasPrefix(mt, "text/") || strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml") {
		return true
	}
	for _, t := range textTypes {
		if mt == t {
			return true
		}
	}
	lower := strings.ToLower(name)
	for _, ext := range textExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}
