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
	fixture = bytes.Replace(fixture, []byte("      operationId: constructor\n"),
		[]byte("      operationId: constructor\n      description: "+strings.Repeat("w", 256<<10)+"\n"), 1)
	compiled, err := compile.Compile(t.Context(), fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, err := json.Marshal(compiled.Document)
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

	page, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tools) != 1 || page.NextCursor == "" {
		t.Fatalf("a page of tools/list went past 256 KiB of tool definitions: %d tools", len(page.Tools))
	}
	if _, err := session.ListTools(t.Context(), &mcp.ListToolsParams{Cursor: "unknown"}); err == nil {
		t.Fatal("tools/list accepted a cursor it never issued")
	}
	var tools []*mcp.Tool
	for tool, err := range session.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tool)
	}
	if len(tools) != 2 || tools[0].Name != "_constructor" || tools[1].Name != "update-widget" || !tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tools/list: %+v", tools)
	}
	schema, err := json.Marshal(tools[0].InputSchema)
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
	callWidget := func(id string, min any) *mcp.CallToolResult {
		t.Helper()
		return call("_constructor", map[string]any{"id": id, "filter": map[string]any{"min": min, "category": nil}})
	}
	if result := callWidget("a/b", json.Number("2e6")); result.IsError || len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "widget" {
		t.Fatalf("tools/call: %+v", result)
	}
	select {
	case request := <-upstreamCalls:
		if request.Method != "GET" || request.URL.EscapedPath() != "/v1/widgets/a%2Fb" ||
			request.URL.Query().Get("filter[min]") != "2000000" || request.Header.Get("X-Widget-Key") != "test-key" {
			t.Fatalf("incorrect upstream request: %s %s", request.Method, request.URL)
		}
	default:
		t.Fatal("successful tool call made no upstream request")
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

	wire, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+
		`"name":"update-widget","arguments":{"id":"a","body":{"name":"next"}},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",`+
		`"io.modelcontextprotocol/clientInfo":{"name":"test","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	wire.Header = http.Header{
		"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"},
		"Mcp-Protocol-Version": {"2026-07-28"}, "Mcp-Method": {"tools/call"}, "Mcp-Name": {"update-widget"},
	}
	response, err := http.DefaultClient.Do(wire)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct{ Result struct{ ResultType string } }
	err = json.NewDecoder(response.Body).Decode(&reply)
	_ = response.Body.Close()
	if err != nil || reply.Result.ResultType != "complete" {
		t.Fatalf("a 2026-07-28 tools/call result went out with resultType %q: %v", reply.Result.ResultType, err)
	}
}
