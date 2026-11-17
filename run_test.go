package om_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/holon-sh/om"
)

func TestGeneratedServer(t *testing.T) {
	if os.Getenv("OM_TEST_NODE") == "" {
		t.Skip("set OM_TEST_NODE=1 to install and run the generated project under Node")
	}
	upstreamCalls := make(chan *http.Request, 8)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls <- r.Clone(r.Context())
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
`), []byte("  - url: "+upstream.URL+"/v1\n"), 1)
	project, err := om.Generate(t.Context(), fixture, nil, om.Options{AppName: "widgets"})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for name, contents := range project.Files {
		if err := os.WriteFile(filepath.Join(directory, name), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	certificate := filepath.Join(directory, "test-ca.pem")
	if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	install := exec.CommandContext(ctx, "npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund")
	install.Dir = directory
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install generated project: %v\n%s", err, output)
	}
	server := exec.CommandContext(ctx, "node", "server.ts")
	server.Dir, server.Stderr = directory, os.Stderr
	server.Env = append(os.Environ(), "HOST=127.0.0.1", "PORT=0", "OM_WIDGET_KEY=test-key", "NODE_EXTRA_CA_CERTS="+certificate)
	stdout, err := server.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = server.Process.Kill()
		_ = server.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("generated server did not start: %v", err)
	}
	origin, found := strings.CutPrefix(strings.TrimSpace(line), "MCP server listening at ")
	if !found {
		t.Fatalf("generated server did not report a listening address: %s", line)
	}
	client := mcpClient{url: origin}
	client.call(t, "initialize", map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"},
	}, nil)

	var list struct {
		Tools []struct {
			Name        string
			InputSchema map[string]json.RawMessage
			Annotations struct{ ReadOnlyHint bool }
		}
	}
	client.call(t, "tools/list", map[string]any{}, &list)
	if len(list.Tools) != 2 || list.Tools[0].Name != "getWidget" || !list.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tools/list: %+v", list)
	}
	if missing := list.Tools[0].InputSchema["$defs"]; !bytes.Contains(missing, []byte(`"exclusiveMinimum":0`)) {
		t.Fatalf("OpenAPI 3.0 bounds did not reach the MCP schema: %s", missing)
	}

	callWidget := func(path string, min int) toolResult {
		t.Helper()
		var result toolResult
		client.call(t, "tools/call", map[string]any{
			"name": "getWidget", "arguments": map[string]any{
				"path": map[string]string{"id": path}, "query": map[string]any{"filter": map[string]any{"min": min, "category": nil}},
			},
		}, &result)
		return result
	}
	if result := callWidget("a/b", 2); result.IsError || len(result.Content) != 1 || result.Content[0].Text != "widget" {
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
	if result := callWidget("redirect", 1); !result.IsError || len(upstreamCalls) != 1 {
		t.Fatalf("upstream redirect was followed or accepted: %+v", result)
	}
}

type mcpClient struct {
	url string
	id  int
}

type toolResult struct {
	IsError bool
	Content []struct{ Text string }
}

func (client *mcpClient) call(t *testing.T, method string, params any, result any) {
	t.Helper()
	client.id++
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": client.id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: HTTP %d: %s", method, response.StatusCode, data)
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		for _, line := range bytes.Split(data, []byte("\n")) {
			if payload, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
				data = payload
				break
			}
		}
	}
	var envelope struct{ Result json.RawMessage }
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Result) == 0 {
		t.Fatalf("%s: expected an MCP result: %s (%v)", method, data, err)
	}
	if result != nil {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			t.Fatal(err)
		}
	}
}
