package om_test

import (
	"bytes"
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
	project, err := om.Generate(t.Context(), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if project.Title != "Widgets" || project.Version != "1" {
		t.Fatalf("wrong identity: %s, %s", project.Title, project.Version)
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
	if len(project.Omitted) != 1 || project.Omitted[0].Path != "/upload" || !strings.Contains(project.Omitted[0].Reason, "multipart/form-data") {
		t.Fatalf("omissions: %+v", project.Omitted)
	}
	if actual := slices.Sorted(maps.Keys(project.Files)); !slices.Equal(actual, []string{"document.json", "package-lock.json", "package.json", "server.ts"}) {
		t.Fatalf("project files: %q", actual)
	}
	second, err := om.Generate(t.Context(), data, nil)
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
components:
  schemas:
    Unused: {type: string, enum: [a], default: b, pattern: '(a)\1'}
`
	base := &url.URL{Scheme: "https", Host: "spec.example.test:8443", Path: "/api/specs/openapi.yaml"}
	for _, version := range []string{"3.0.4", "3.1.1", "3.2.0"} {
		t.Run(version, func(t *testing.T) {
			project, err := om.Generate(t.Context(), []byte("openapi: "+version+api), base)
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
		{"No info", "openapi: '3.1.1'" + strings.Replace(api, "info:", "x-info:", 1), "info"},
		{"HTTP server", "openapi: '3.1.1'" + strings.Replace(api, "../v2", "http://example.test", 1), "HTTPS"},
		{"External reference", "openapi: '3.1.1'" + strings.Replace(api, "operationId: listWidgets", "requestBody: {$ref: 'https://example.test/body.yaml'}", 1), "external"},
		{"No tools", "openapi: '3.1.1'" + strings.Replace(api, "get:\n", "trace:\n", 1), "no supported operations"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := om.Generate(t.Context(), []byte(test.doc), base)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected an error containing %q, got %v", test.want, err)
			}
		})
	}
}

func TestOmissions(t *testing.T) {
	for _, test := range []struct {
		name    string
		doc     string
		omitted string
	}{
		{
			"tool name already used",
			`paths:
  /first:
    get:
      operationId: "get:first"
  /second:
    get:
      operationId: "get/first"
`,
			"GET /second",
		},
		{
			"credential and server variable share an environment variable",
			`paths:
  /first:
    get:
      security: [{api_key: []}]
  /second:
    get: {}
components:
  securitySchemes:
    api_key: {type: apiKey, name: X-Test, in: header}
`,
			"GET /first",
		},
		{
			"parameters share a name across locations",
			`paths:
  /first/{id}:
    get:
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
        - {name: id, in: query, schema: {type: string}}
  /second:
    get: {}
`,
			"GET /first/{id}",
		},
		{
			"parameter named body beside a request body",
			`paths:
  /first:
    post:
      parameters: [{name: body, in: query, schema: {type: string}}]
      requestBody: {content: {application/json: {schema: {type: object}}}}
  /second:
    get: {}
`,
			"POST /first",
		},
		{
			"unsupported document schema dialect",
			`jsonSchemaDialect: https://example.test/dialect
paths:
  /first:
    get:
      parameters: [{name: q, in: query, schema: {type: string}}]
  /second:
    get: {}
`,
			"GET /first",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := fmt.Sprintf(`openapi: '3.1.1'
info: {title: Test, version: '1'}
servers:
  - url: 'https://{api-key}.example.test'
    variables:
      api-key: {default: eu}
%s`, test.doc)
			project, err := om.Generate(t.Context(), []byte(document), nil)
			if err != nil {
				t.Fatal(err)
			}
			var omitted []string
			for _, omission := range project.Omitted {
				omitted = append(omitted, omission.Method+" "+omission.Path)
			}
			if !slices.Equal(omitted, []string{test.omitted}) {
				t.Fatalf("omitted %q, want [%q]", omitted, test.omitted)
			}
		})
	}
}
