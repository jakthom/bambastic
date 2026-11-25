// Package bridge exposes the production workflow through the Model Context Protocol.
package bridge

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type object = map[string]any

//go:embed catalog.json
var catalogData []byte

type operation struct {
	Name, Category, Method, Path, Description string
	Query                                     []string
	Input                                     json.RawMessage `json:"input_schema"`
	check                                     *jsonschema.Resolved
}
type invoke func(*http.Request) (*http.Response, error)
type Catalog struct {
	ops     map[string]operation
	ordered []operation
}

func New() (*Catalog, error) {
	var list []operation
	if e := json.Unmarshal(catalogData, &list); e != nil {
		return nil, e
	}
	c := &Catalog{ops: map[string]operation{}}
	for _, op := range list {
		var schema jsonschema.Schema
		if e := json.Unmarshal(op.Input, &schema); e != nil {
			return nil, e
		}
		v, e := schema.Resolve(nil)
		if e != nil {
			return nil, e
		}
		op.check = v
		c.ops[op.Name] = op
		c.ordered = append(c.ordered, op)
	}
	sort.Slice(c.ordered, func(i, j int) bool { return c.ordered[i].Name < c.ordered[j].Name })
	return c, nil
}

type search struct {
	Query    string `json:"query,omitempty"`
	Category string `json:"category,omitempty"`
}
type execute struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

func payload(v any) *mcp.CallToolResult {
	b, _ := json.Marshal(v)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: object{"result": v}}
}
func failure(name, code string, status int, reason any) *mcp.CallToolResult {
	detail := object{"operation": name, "code": code, "detail": reason}
	text := fmt.Sprint(reason)
	if status > 0 {
		detail["http_status"] = status
		text = fmt.Sprintf("HTTP %d: %s", status, text)
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: object{"error": detail}}
}
func (c *Catalog) server(call invoke) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "bambastic", Version: "0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "list_categories", Description: "List categories of registered Bambastic actions", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		counts := map[string]int{}
		for _, op := range c.ordered {
			counts[op.Category]++
		}
		return payload(counts), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "search_tools", Description: "Find operations by keyword or category and inspect their argument schemas", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *mcp.CallToolRequest, in search) (*mcp.CallToolResult, any, error) {
		out := []object{}
		for _, op := range c.ordered {
			if in.Category != "" && op.Category != in.Category {
				continue
			}
			terms := strings.Fields(strings.ToLower(in.Query))
			hay := strings.ToLower(op.Name + " " + op.Category + " " + op.Description)
			ok := true
			for _, term := range terms {
				ok = ok && strings.Contains(hay, term)
			}
			if ok {
				out = append(out, object{"name": op.Name, "category": op.Category, "description": op.Description, "read_only": op.Method == "GET", "input_schema": op.Input})
			}
		}
		return payload(out), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "execute_tool", Description: "Execute a discovered operation by name with its arguments using the caller's rights"}, func(ctx context.Context, req *mcp.CallToolRequest, in execute) (*mcp.CallToolResult, any, error) {
		return c.run(ctx, call, in), nil, nil
	})
	return s
}
func (c *Catalog) run(ctx context.Context, call invoke, in execute) *mcp.CallToolResult {
	op, ok := c.ops[in.Name]
	if !ok {
		return failure(in.Name, "unknown_operation", 0, "Unknown operation")
	}
	if in.Arguments == nil {
		in.Arguments = object{}
	}
	if e := op.check.Validate(in.Arguments); e != nil {
		return failure(op.Name, "invalid_arguments", 0, e.Error())
	}
	path := op.Path
	q := url.Values{}
	body := object{}
	key := ""
	for k, v := range in.Arguments {
		holder := "{" + k + "}"
		if strings.Contains(path, holder) {
			path = strings.ReplaceAll(path, holder, url.PathEscape(fmt.Sprint(v)))
			continue
		}
		isQuery := false
		for _, p := range op.Query {
			if k == p {
				isQuery = true
				break
			}
		}
		if isQuery {
			q.Set(k, fmt.Sprint(v))
		} else if k == "request_key" {
			key, _ = v.(string)
		} else {
			body[k] = v
		}
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	b, _ := json.Marshal(body)
	var reader io.Reader
	if op.Method != "GET" {
		reader = bytes.NewReader(b)
	}
	r, e := http.NewRequestWithContext(ctx, op.Method, path, reader)
	if e != nil {
		return failure(op.Name, "invalid_operation", 0, "Invalid operation")
	}
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	res, e := call(r)
	if e != nil {
		return failure(op.Name, "unavailable", 0, "Bambastic server unavailable")
	}
	defer res.Body.Close()
	const max = 12 << 20
	data, e := io.ReadAll(io.LimitReader(res.Body, max+1))
	if e != nil {
		return failure(op.Name, "incomplete_response", 0, "Connection closed while reading the result")
	}
	if len(data) > max {
		return failure(op.Name, "response_limit", 0, "Response exceeds MCP result limit")
	}
	if res.StatusCode >= 300 {
		var problem object
		json.Unmarshal(data, &problem)
		reason := problem["detail"]
		if reason == nil {
			reason = http.StatusText(res.StatusCode)
		}
		redact(reason)
		return failure(op.Name, strings.ReplaceAll(strings.ToLower(http.StatusText(res.StatusCode)), " ", "_"), res.StatusCode, reason)
	}
	mime := strings.Split(res.Header.Get("Content-Type"), ";")[0]
	if op.Name == "camera_snapshot" {
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
			return failure(op.Name, "invalid_media", 0, "Camera returned an unsupported media type")
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: mime}}}
	}
	var val any
	if len(bytes.TrimSpace(data)) > 0 {
		if e = json.Unmarshal(data, &val); e != nil {
			return failure(op.Name, "invalid_response", 0, "Unexpected response format")
		}
	}
	redact(val)
	return payload(val)
}
func redact(v any) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			redact(e)
		}
	case map[string]any:
		for k, item := range x {
			low := strings.ToLower(k)
			if strings.Contains(low, "password") || strings.Contains(low, "access_code") || strings.Contains(low, "token") || strings.Contains(low, "secret") || strings.Contains(low, "api_key") || strings.HasSuffix(low, "camera_url") || strings.HasSuffix(low, "snapshot_url") {
				delete(x, k)
			} else {
				redact(item)
			}
		}
	}
}
func (c *Catalog) HTTP(api http.Handler, publicURL string) http.Handler {
	server := func(r *http.Request) *mcp.Server {
		ctx := r.Context()
		return c.server(func(req *http.Request) (*http.Response, error) {
			out := httptest.NewRecorder()
			api.ServeHTTP(out, req.WithContext(ctx))
			return out.Result(), nil
		})
	}
	opts := mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}
	direct := mcp.NewStreamableHTTPHandler(server, &opts)
	origin, err := url.Parse(publicURL)
	if err != nil || (origin.Scheme != "https" && origin.Scheme != "http") ||
		origin.Hostname() == "" || origin.User != nil || (origin.Path != "" && origin.Path != "/") ||
		origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return direct
	}

	// Tailscale Serve preserves the HTTPS authority when forwarding over loopback.
	// Only the operator's exact authority substitutes for the SDK's localhost check.
	proxyOpts := opts
	proxyOpts.DisableLocalhostProtection = true
	proxied := mcp.NewStreamableHTTPHandler(server, &proxyOpts)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local, _ := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		loopback := local != nil && local.IP.IsLoopback() && err == nil && peer.Addr().Unmap().IsLoopback()
		if !loopback || !strings.EqualFold(r.Host, origin.Host) {
			direct.ServeHTTP(w, r)
			return
		}
		if supplied := r.Header.Values("Origin"); len(supplied) > 0 &&
			(len(supplied) != 1 || !strings.EqualFold(supplied[0], origin.Scheme+"://"+origin.Host)) {
			http.Error(w, "Cross-origin request refused", http.StatusForbidden)
			return
		}
		proxied.ServeHTTP(w, r)
	})
}
func (c *Catalog) Stdio(ctx context.Context, address, token, diagnosticsRoot string) error {
	base, e := url.Parse(address)
	if e != nil || base.Host == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") {
		return errors.New("provide a valid Bambastic HTTP(S) server URL")
	}
	client := http.Client{Timeout: 90 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	s := c.server(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = base.Scheme
		req.URL.Host = base.Host
		req.URL.Path = strings.TrimRight(base.Path, "/") + req.URL.Path
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return client.Do(req)
	})
	if diagnosticsRoot != "" {
		addUIResources(s, diagnosticsRoot)
	}
	return s.Run(ctx, &mcp.StdioTransport{})
}
