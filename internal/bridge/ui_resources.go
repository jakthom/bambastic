package bridge

import (
	"context"

	"bambastic/internal/uidiagnostics"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func addUIResources(s *mcp.Server, root string) {
	for _, name := range []string{"guide", "uie", "crashes"} {
		uri := "bambastic://ui-diagnostics/" + name
		mime := "application/x-ndjson"
		if name == "guide" {
			mime = "text/markdown"
		}
		s.AddResource(&mcp.Resource{URI: uri, Name: name, MIMEType: mime, Description: "Local development UI diagnostics: " + name}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			text := uidiagnostics.Guide
			if name != "guide" {
				var err error
				text, err = uidiagnostics.Tail(root, name)
				if err != nil {
					return nil, err
				}
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Text: text}}}, nil
		})
	}
}
