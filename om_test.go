package om_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/holon-sh/om"
)

func TestProjectContract(t *testing.T) {
	data, err := os.ReadFile("testdata/widgets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	project, err := om.Generate(t.Context(), data, nil, om.Options{AppName: "widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if project.Name != "widgets" || project.Title != "Widgets" || project.Version != "1" {
		t.Fatalf("wrong identity: %s, %s, %s", project.Name, project.Title, project.Version)
	}
	if project.Endpoint != (om.Endpoint{Host: "{OM_REGION}.example.test", Port: 443}) {
		t.Fatalf("endpoint: %+v", project.Endpoint)
	}
	if len(project.Inputs) != 2 {
		t.Fatalf("inputs: %+v", project.Inputs)
	}
	region, key := project.Inputs[0], project.Inputs[1]
	if region.Name != "OM_REGION" || region.Secret || region.Default == nil || *region.Default != "us" || !slices.Equal(region.Choices, []string{"us", "eu"}) {
		t.Fatalf("region input: %+v", region)
	}
	if key.Name != "OM_WIDGET_KEY" || !key.Secret || key.Default != nil {
		t.Fatalf("key input: %+v", key)
	}
	if len(project.Omitted) != 1 || project.Omitted[0].Path != "/upload" || !strings.Contains(project.Omitted[0].Reason, "request body") {
		t.Fatalf("omissions: %+v", project.Omitted)
	}
	if actual := slices.Sorted(maps.Keys(project.Files)); !slices.Equal(actual, []string{"package-lock.json", "package.json", "server.ts"}) {
		t.Fatalf("project files: %q", actual)
	}
	var pkg struct {
		Name         string
		Dependencies map[string]string
	}
	if err := json.Unmarshal(project.Files["package.json"], &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "widgets" || len(pkg.Dependencies) != 2 || pkg.Dependencies["@modelcontextprotocol/server"] == "" || pkg.Dependencies["@modelcontextprotocol/node"] == "" {
		t.Fatalf("portable package: %+v", pkg)
	}
	second, err := om.Generate(t.Context(), data, nil, om.Options{AppName: "widgets"})
	if err != nil || !maps.EqualFunc(project.Files, second.Files, bytes.Equal) {
		t.Fatalf("generate the same document twice: %v", err)
	}
}

func TestDocumentBoundary(t *testing.T) {
	const api = `
info: {title: Widgets, version: '1'}
servers: [{url: '../v2'}]
paths:
  /widgets:
    get:
      operationId: listWidgets
      responses:
        '200': {description: ok}
`
	base := &url.URL{Scheme: "https", Host: "spec.example.test:8443", Path: "/api/specs/openapi.yaml"}
	for _, version := range []string{"3.0.4", "3.1.1", "3.2.0"} {
		t.Run(version, func(t *testing.T) {
			project, err := om.Generate(t.Context(), []byte("openapi: "+version+api), base, om.Options{AppName: "widgets"})
			if err != nil {
				t.Fatal(err)
			}
			if project.Endpoint != (om.Endpoint{Host: "spec.example.test", Port: 8443}) {
				t.Fatalf("endpoint: %+v", project.Endpoint)
			}
		})
	}
	for _, test := range []struct {
		name string
		doc  string
		want string
	}{
		{"Swagger", "swagger: '2.0'" + api, "OpenAPI 3.0"},
		{"HTTP server", "openapi: '3.1.1'" + strings.Replace(api, "../v2", "http://example.test", 1), "HTTPS"},
		{"External reference", "openapi: '3.1.1'" + strings.Replace(api, "operationId: listWidgets", "requestBody: {$ref: 'https://example.test/body.yaml'}", 1), "external"},
		{"Schema dialect", "openapi: '3.1.1'\njsonSchemaDialect: https://example.test/dialect" + api, "dialect is unsupported"},
		{"No tools", "openapi: '3.1.1'" + strings.Replace(api, "get:\n", "trace:\n", 1), "no supported operations"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := om.Generate(context.Background(), []byte(test.doc), base, om.Options{AppName: "widgets"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected an error containing %q, got %v", test.want, err)
			}
		})
	}
}

func TestNamesDoNotSilentlyCollide(t *testing.T) {
	for _, test := range []struct {
		name string
		doc  string
	}{
		{
			"tool names",
			`servers: [{url: 'https://example.test'}]
paths:
  /first:
    get:
      operationId: "get:first"
  /second:
    get:
      operationId: "get/first"
`,
		},
		{
			"credential and server variable",
			`servers:
  - url: 'https://{api-key}.example.test'
    variables:
      api-key: {default: eu}
paths:
  /first:
    get:
      security: [{api_key: []}]
components:
  securitySchemes:
    api_key: {type: apiKey, name: X-Test, in: header}
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := fmt.Sprintf("openapi: '3.1.1'\ninfo: {title: Test, version: '1'}\n%s", test.doc)
			_, err := om.Generate(t.Context(), []byte(document), nil, om.Options{AppName: "test"})
			if err == nil || !strings.Contains(err.Error(), "map to") {
				t.Fatalf("expected a collision, got %v", err)
			}
		})
	}
}
