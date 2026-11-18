package om

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Format = 1

// Document is a compiled OpenAPI document: one MCP tool per operation.
// Its JSON encoding is the model a host stores between compiling and serving.
type Document struct {
	Format      int         `json:"om"`
	Title       string      `json:"title"`
	Version     string      `json:"version"`
	Description string      `json:"description,omitempty"`
	Server      string      `json:"server"`
	Inputs      []Input     `json:"inputs"`
	Operations  []Operation `json:"operations"`
}

type Input struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Default     *string  `json:"default,omitempty"`
	Choices     []string `json:"choices,omitempty"`
	Secret      bool     `json:"secret,omitempty"`
}

type Endpoint struct {
	Host string
	Port int
}

type Operation struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Parameters  []Parameter     `json:"parameters,omitempty"`
	Body        bool            `json:"body,omitempty"`
	Security    Security        `json:"security,omitempty"`
}

type Parameter struct {
	Name    string   `json:"name"`
	In      Location `json:"in"`
	Explode bool     `json:"explode,omitempty"`
}

// Location is where an argument is sent, serialized in the style OpenAPI defines for it there:
// simple in a path or header, form in a query or cookie, and deepObject for a query object.
// https://spec.openapis.org/oas/v3.2.0.html#style-values
type Location string

const (
	Path       Location = "path"
	Header     Location = "header"
	Query      Location = "query"
	DeepObject Location = "deepObject"
	Cookie     Location = "cookie"
)

func (l *Location) UnmarshalText(text []byte) error {
	*l = Location(text)
	if !slices.Contains([]Location{Path, Header, Query, DeepObject, Cookie}, *l) {
		return fmt.Errorf("unknown parameter location %q", text)
	}
	return nil
}

type Credential interface {
	wire() credential
	attach(*upstream, func(string) (string, bool)) error
}

type APIKey struct {
	In    Location
	Name  string
	Input string
}

type Bearer struct {
	Input string
}

type Basic struct {
	Username string
	Password string
}

type Security []Credential

type credential struct {
	Kind     string   `json:"kind"`
	In       Location `json:"in,omitempty"`
	Name     string   `json:"name,omitempty"`
	Input    string   `json:"input,omitempty"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
}

func (k APIKey) wire() credential {
	return credential{Kind: "apiKey", In: k.In, Name: k.Name, Input: k.Input}
}
func (b Bearer) wire() credential { return credential{Kind: "bearer", Input: b.Input} }
func (b Basic) wire() credential {
	return credential{Kind: "basic", Username: b.Username, Password: b.Password}
}

func (s Security) MarshalJSON() ([]byte, error) {
	wire := make([]credential, len(s))
	for i, c := range s {
		wire[i] = c.wire()
	}
	return json.Marshal(wire)
}

func (s *Security) UnmarshalJSON(data []byte) error {
	var wire []credential
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*s = make(Security, len(wire))
	for i, c := range wire {
		switch {
		case c.Kind == "apiKey" && slices.Contains([]Location{Header, Query, Cookie}, c.In):
			(*s)[i] = APIKey{c.In, c.Name, c.Input}
		case c.Kind == "bearer":
			(*s)[i] = Bearer{c.Input}
		case c.Kind == "basic":
			(*s)[i] = Basic{c.Username, c.Password}
		default:
			return fmt.Errorf("unknown credential %q in %q", c.Kind, c.In)
		}
	}
	return nil
}

// Endpoint is the host and port Server reaches, derived from it.
func (d Document) Endpoint() Endpoint {
	authority, _, _ := strings.Cut(strings.TrimPrefix(d.Server, "https://"), "/")
	address := url.URL{Host: authority}
	port := 443
	if address.Port() != "" {
		port, _ = strconv.Atoi(address.Port())
	}
	return Endpoint{Host: address.Hostname(), Port: port}
}

// Handler serves the document's operations as MCP tools over Streamable HTTP, statelessly.
// lookup supplies an input's value by name; transport carries the upstream requests.
func (d Document) Handler(lookup func(name string) (string, bool), transport http.RoundTripper) (http.Handler, error) {
	if d.Format != Format {
		return nil, fmt.Errorf("model format %d is not served by this version of om, which serves format %d; compile the document again", d.Format, Format)
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       30 * time.Second,
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return served{d, lookup, client}.server()
	}, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, PropagateRequestCancellation: true, Logger: slog.Default(),
	}), nil
}

type served struct {
	Document
	lookup func(string) (string, bool)
	client *http.Client
}

func (s served) server() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: s.Title, Title: s.Title, Version: s.Version}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			switch request := request.(type) {
			case *mcp.ListToolsRequest:
				return s.list(request.Params)
			case *mcp.CallToolRequest:
				if i := slices.IndexFunc(s.Operations, func(o Operation) bool { return o.Name == request.Params.Name }); i >= 0 {
					return s.call(ctx, s.Operations[i], request.Params.Arguments), nil
				}
			}
			return next(ctx, method, request)
		}
	})
	return server
}

const pageBytes = 256 << 10

func (s served) list(params *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	start := 0
	if params != nil && params.Cursor != "" {
		var err error
		start, err = strconv.Atoi(params.Cursor)
		if err != nil || start <= 0 || start >= len(s.Operations) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("invalid cursor %q", params.Cursor)}
		}
	}
	result := &mcp.ListToolsResult{Tools: []*mcp.Tool{}, Cacheable: mcp.Cacheable{CacheScope: "public"}}
	size := 0
	for i, operation := range s.Operations[start:] {
		size += len(operation.Name) + len(operation.Title) + len(operation.Description) + len(operation.InputSchema)
		if i > 0 && size > pageBytes {
			result.NextCursor = strconv.Itoa(start + i)
			break
		}
		result.Tools = append(result.Tools, operation.Tool())
	}
	return result, nil
}

// Tool is the MCP tool the operation is served as.
func (o Operation) Tool() *mcp.Tool {
	tool := &mcp.Tool{Name: o.Name, Title: o.Title, Description: o.Description, InputSchema: o.InputSchema}
	switch o.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		tool.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
	case http.MethodPut, http.MethodDelete:
		tool.Annotations = &mcp.ToolAnnotations{IdempotentHint: true}
	}
	return tool
}
