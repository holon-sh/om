package om

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Format = 2

// Document is a compiled OpenAPI document: one MCP tool per operation.
// Its JSON encoding is the model a host stores between compiling and serving.
type Document struct {
	Format      int                        `json:"om"`
	Title       string                     `json:"title"`
	Version     string                     `json:"version"`
	Description string                     `json:"description,omitempty"`
	Server      string                     `json:"server"`
	Inputs      []Input                    `json:"inputs"`
	Definitions map[string]json.RawMessage `json:"definitions,omitempty"`
	Operations  []Operation                `json:"operations"`
}

// Input is a value the host supplies by name: a server variable when it has a default, a credential when it has none.
// A credential with Authorization is an OAuth 2.0 access token, which operations send as an [RFC 6750 §2.1] bearer
// token.
//
// [RFC 6750 §2.1]: https://www.rfc-editor.org/rfc/rfc6750.html#section-2.1
type Input struct {
	Name          string         `json:"name"`
	Default       *string        `json:"default,omitempty"`
	Choices       []string       `json:"choices,omitempty"`
	Authorization *Authorization `json:"authorization,omitempty"`
}

// Authorization is the [RFC 8414 §2] issuer of the authorization server that issues an access token and the scopes the
// operations that send it require.
//
// [RFC 8414 §2]: https://www.rfc-editor.org/rfc/rfc8414.html#section-2
type Authorization struct {
	Issuer string   `json:"issuer"`
	Scopes []string `json:"scopes,omitempty"`
}

type Operation struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Parameters   []Parameter     `json:"parameters,omitempty"`
	Body         bool            `json:"body,omitempty"`
	Security     Security        `json:"security,omitempty"`
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

// Handler serves the document's operations as MCP tools over Streamable HTTP, statelessly.
// lookup supplies an input's value by name; transport carries the upstream requests.
func (d Document) Handler(lookup func(ctx context.Context, name string) (string, bool), transport http.RoundTripper) (http.Handler, error) {
	server, err := served{d, lookup, &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       30 * time.Second,
	}}.server()
	if err != nil {
		return nil, err
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, PropagateRequestCancellation: true, Logger: slog.Default(),
	}), nil
}

// Implementation is the MCP server om serves the document as and the options it serves it with. It refuses a model of a
// format this version of om does not serve.
func (d Document) Implementation() (*mcp.Implementation, *mcp.ServerOptions, error) {
	if d.Format != Format {
		return nil, nil, fmt.Errorf("model format %d is not served by this version of om, which serves format %d; compile the document again", d.Format, Format)
	}
	return &mcp.Implementation{Name: d.Title, Title: d.Title, Version: d.Version},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}, SupportedProtocolVersions: []string{"2026-07-28"}}, nil
}

type served struct {
	Document
	lookup func(context.Context, string) (string, bool)
	client *http.Client
}

// server pages tools/list itself and answers tools/call through the SDK's tool dispatch, which gives the result the
// resultType that [MCP 2026-07-28] clients require.
//
// [MCP 2026-07-28]: https://modelcontextprotocol.io/specification/2026-07-28/server/tools
func (s served) server() (*mcp.Server, error) {
	implementation, options, err := s.Implementation()
	if err != nil {
		return nil, err
	}
	server := mcp.NewServer(implementation, options)
	tools, err := s.tools()
	if err != nil {
		return nil, err
	}
	for _, t := range tools {
		if err := addTool(server, &mcp.Tool{Name: t.Name, InputSchema: t.InputSchema}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.call(ctx, t, request.Params.Arguments), nil
		}); err != nil {
			return nil, err
		}
	}
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if request, ok := request.(*mcp.ListToolsRequest); ok {
				if request.Params == nil {
					return s.Tools("")
				}
				return s.Tools(request.Params.Cursor)
			}
			return next(ctx, method, request)
		}
	})
	return server, nil
}

func addTool(server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandler) (err error) {
	defer func() {
		if refused := recover(); refused != nil {
			err = fmt.Errorf("%v", refused)
		}
	}()
	server.AddTool(tool, handler)
	return nil
}

// pageBytes bounds the name, title, description and schema bytes of a tools/list page.
const pageBytes = 256 << 10

// Tools is the page of tools a tools/list cursor names. A cursor Tools did not issue is [invalid params].
//
// [invalid params]: https://modelcontextprotocol.io/specification/2026-07-28/server/utilities/pagination#error-handling
func (d Document) Tools(cursor string) (*mcp.ListToolsResult, error) {
	start := 0
	if cursor != "" {
		var err error
		start, err = strconv.Atoi(cursor)
		if err != nil || start <= 0 || start >= len(d.Operations) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("invalid cursor %q", cursor)}
		}
	}
	result := &mcp.ListToolsResult{Tools: []*mcp.Tool{}, Cacheable: mcp.Cacheable{CacheScope: "public"}}
	size := 0
	for i, operation := range d.Operations[start:] {
		operation, err := d.standalone(operation)
		if err != nil {
			return nil, err
		}
		size += len(operation.Name) + len(operation.Title) + len(operation.Description) + len(operation.InputSchema) + len(operation.OutputSchema)
		if i > 0 && size > pageBytes {
			result.NextCursor = strconv.Itoa(start + i)
			break
		}
		result.Tools = append(result.Tools, operation.tool())
	}
	return result, nil
}

// Tool is the MCP tool the operation is served as.
func (d Document) Tool(o Operation) (*mcp.Tool, error) {
	o, err := d.standalone(o)
	if err != nil {
		return nil, err
	}
	return o.tool(), nil
}

func (o Operation) tool() *mcp.Tool {
	tool := &mcp.Tool{Name: o.Name, Title: o.Title, Description: o.Description, InputSchema: o.InputSchema}
	if o.OutputSchema != nil {
		tool.OutputSchema = o.OutputSchema
	}
	switch o.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		tool.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
	case http.MethodPut, http.MethodDelete:
		tool.Annotations = &mcp.ToolAnnotations{IdempotentHint: true}
	}
	return tool
}
