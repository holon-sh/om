package om_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/holon-sh/om"
	"github.com/holon-sh/om/compile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHandler(t *testing.T) {
	upstreamCalls := make(chan *http.Request, 8)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contents, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured := r.Clone(r.Context())
		captured.Body = io.NopCloser(bytes.NewReader(contents))
		upstreamCalls <- captured
		if strings.HasSuffix(r.URL.Path, "/redirect") {
			http.Redirect(w, r, "/v1/widgets/elsewhere", http.StatusFound)
			return
		}
		fmt.Fprint(w, "widget")
	}))
	defer upstream.Close()
	fixture, err := os.ReadFile("testdata/widgets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fixture = bytes.Replace(fixture, []byte(`  - url: https://{region}.example.test/v1
    variables:
      region:
        default: us
        enum: [us, eu]
`), []byte("  - url: "+upstream.URL+"/{version}\n    variables:\n      version: {default: v1}\n"), 1)
	compiled, _, err := compile.Compile(t.Context(), fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, err := json.Marshal(compiled)
	if err != nil {
		t.Fatal(err)
	}
	var document om.Document
	if err := json.Unmarshal(model, &document); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) { return "test-key", name == "OM_WIDGET_KEY" }
	if _, err := (om.Document{Format: om.Format + 1}).Handler(lookup, upstream.Client().Transport); err == nil {
		t.Fatal("served a model format this version does not know")
	}
	for _, unknown := range []string{`{"parameters":[{"name":"id","in":"body"}]}`, `{"security":[{"kind":"oauth2"}]}`} {
		if err := json.Unmarshal([]byte(unknown), &om.Operation{}); err == nil {
			t.Fatalf("decoded an operation this version cannot serve: %s", unknown)
		}
	}
	handler, err := document.Handler(lookup, upstream.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	list, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 2 || list.Tools[0].Name != "_constructor" || !list.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tools/list: %+v", list.Tools)
	}
	schema, err := json.Marshal(list.Tools[0].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(schema, []byte(`"description":"Widget identifier"`)) {
		t.Fatalf("parameter description did not reach the MCP schema: %s", schema)
	}
	if !bytes.Contains(schema, []byte(`"exclusiveMinimum":0`)) {
		t.Fatalf("OpenAPI 3.0 bounds did not reach the MCP schema: %s", schema)
	}

	call := func(name string, arguments map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	callWidget := func(id string, min int) *mcp.CallToolResult {
		t.Helper()
		return call("_constructor", map[string]any{"id": id, "filter": map[string]any{"min": min, "category": nil}})
	}
	if result := callWidget("a/b", 2); result.IsError || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "widget" {
		t.Fatalf("tools/call: %+v", result)
	}
	select {
	case request := <-upstreamCalls:
		if request.Method != "GET" || request.URL.EscapedPath() != "/v1/widgets/a%2Fb" ||
			request.URL.Query().Get("filter[min]") != "2" || request.Header.Get("X-Widget-Key") != "test-key" {
			t.Fatalf("incorrect upstream request: %s %s", request.Method, request.URL)
		}
	default:
		t.Fatal("successful tool call made no upstream request")
	}
	if result := callWidget("invalid", 0); !result.IsError || len(upstreamCalls) != 0 {
		t.Fatalf("schema violation reached the upstream: %+v", result)
	}
	if result := callWidget("A1", 2); !result.IsError || len(upstreamCalls) != 0 {
		t.Fatalf("pattern violation reached the upstream: %+v", result)
	}
	if result := call("update-widget", map[string]any{"id": "a", "body": map[string]string{"name": "next"}}); result.IsError {
		t.Fatalf("JSON body rejected: %+v", result)
	}
	select {
	case request := <-upstreamCalls:
		body, err := io.ReadAll(request.Body)
		if err != nil || request.Method != "POST" || string(body) != `{"name":"next"}` ||
			request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("incorrect upstream JSON request: %s %s: %v", request.Method, body, err)
		}
	default:
		t.Fatal("successful update made no upstream request")
	}
	if result := callWidget("redirect", 1); !result.IsError || len(upstreamCalls) != 1 {
		t.Fatalf("upstream redirect was followed or accepted: %+v", result)
	}
}
