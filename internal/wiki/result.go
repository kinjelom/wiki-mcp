package wiki

import (
	"bytes"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// JSONResult is a tool result holding v as compact JSON text.
func JSONResult(v any) (*mcp.CallToolResult, error) {
	text, err := JSONText(v)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
}

// JSONText encodes v as compact JSON without HTML escaping (page titles and snippets stay readable).
func JSONText(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(b.Bytes(), "\n")), nil
}

// Ptr returns a pointer to v, for the optional fields of the MCP types.
func Ptr[T any](v T) *T { return &v }
