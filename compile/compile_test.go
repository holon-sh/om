package compile_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/holon-sh/om"
	"github.com/holon-sh/om/compile"
)

func TestCompile(t *testing.T) {
	data, err := os.ReadFile("../testdata/widgets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compile.Compile(t.Context(), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, omitted := compiled.Document, compiled.Omitted
	if document.Title != "Widgets" || document.Version != "1" {
		t.Fatalf("wrong identity: %s, %s", document.Title, document.Version)
	}
	if document.Server != "https://{OM_REGION}.example.test/v1" {
		t.Fatalf("server: %s", document.Server)
	}
	if len(document.Inputs) != 2 {
		t.Fatalf("inputs: %+v", document.Inputs)
	}
	region, key := document.Inputs[0], document.Inputs[1]
	if region.Name != "OM_REGION" || region.Default == nil || *region.Default != "us" || !slices.Equal(region.Choices, []string{"us", "eu"}) {
		t.Fatalf("region input: %+v", region)
	}
	if key.Name != "OM_WIDGET_KEY" || key.Default != nil {
		t.Fatalf("key input: %+v", key)
	}
	if len(omitted) != 1 || omitted[0].Path != "/upload" {
		t.Fatalf("omissions: %+v", omitted)
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
	compiled, err := compile.Compile(t.Context(), []byte("openapi: '3.1.1'"+api), base)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Document.Server != "https://spec.example.test:8443/api/v2" {
		t.Fatalf("server: %s", compiled.Document.Server)
	}
	for _, test := range []struct{ name, doc string }{
		{"Swagger", "swagger: '2.0'" + api},
		{"No info", "openapi: '3.1.1'" + strings.Replace(api, "info:", "x-info:", 1)},
		{"HTTP server", "openapi: '3.1.1'" + strings.Replace(api, "../v2", "http://example.test", 1)},
		{"No tools", "openapi: '3.1.1'" + strings.Replace(api, "get:\n", "trace:\n", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := compile.Compile(t.Context(), []byte(test.doc), base); err == nil {
				t.Fatal("compiled a document outside the boundary")
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
			"OAuth 2.0 scheme that names no metadata",
			`paths:
  /first:
    get:
      security: [{sign-in: []}]
  /second:
    get: {}
components:
  securitySchemes:
    sign-in:
      type: oauth2
      flows: {authorizationCode: {authorizationUrl: 'https://as.example.test/authorize', tokenUrl: 'https://as.example.test/token', scopes: {}}}
`,
			"GET /first",
		},
		{
			"OpenID Connect scheme whose metadata URL is relative",
			`paths:
  /first:
    get:
      security: [{sign-in: []}]
  /second:
    get: {}
components:
  securitySchemes:
    sign-in: {type: openIdConnect, openIdConnectUrl: '/.well-known/openid-configuration'}
`,
			"GET /first",
		},
		{
			"OAuth 2.0 scheme named like another scheme's credential",
			`paths:
  /first:
    get:
      security: [{login: []}]
  /second:
    get:
      security: [{login_USERNAME: []}]
components:
  securitySchemes:
    login: {type: http, scheme: basic}
    login_USERNAME: {type: oauth2, oauth2MetadataUrl: 'https://as.example.test/.well-known/oauth-authorization-server', flows: {}}
`,
			"GET /second",
		},
		{
			"OpenID Connect scheme whose metadata is not at a well-known path",
			`paths:
  /first:
    get:
      security: [{sign-in: []}]
  /second:
    get: {}
components:
  securitySchemes:
    sign-in: {type: openIdConnect, openIdConnectUrl: 'https://id.example.test/openid-configuration'}
`,
			"GET /first",
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
			compiled, err := compile.Compile(t.Context(), []byte(document), nil)
			if err != nil {
				t.Fatal(err)
			}
			var omitted []string
			for _, omission := range compiled.Omitted {
				omitted = append(omitted, omission.Method+" "+omission.Path)
			}
			if !slices.Equal(omitted, []string{test.omitted}) {
				t.Fatalf("omitted %q, want [%q]", omitted, test.omitted)
			}
		})
	}
}

func TestOAuthSchemes(t *testing.T) {
	const api = `openapi: 3.2.0
info: {title: Test, version: '1'}
servers: [{url: 'https://api.example.test'}]
paths:
  /items:
    get:
      security: [{sign-in: [items.read]}]
    post:
      security: [{sign-in: [items.write]}]
components:
  securitySchemes:
    sign-in: %s
`
	for _, test := range []struct {
		name, scheme, issuer string
	}{
		{"OAuth 2.0", `{type: oauth2, oauth2MetadataUrl: 'https://as.example.test/.well-known/oauth-authorization-server/tenant', flows: {}}`, "https://as.example.test/tenant"},
		{"OpenID Connect", `{type: openIdConnect, openIdConnectUrl: 'https://id.example.test/tenant/.well-known/openid-configuration'}`, "https://id.example.test/tenant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := compile.Compile(t.Context(), []byte(fmt.Sprintf(api, test.scheme)), nil)
			if err != nil {
				t.Fatal(err)
			}
			document := compiled.Document
			want := []om.Input{{Name: "OM_SIGN_IN", Authorization: &om.Authorization{Issuer: test.issuer, Scopes: []string{"items.read", "items.write"}}}}
			if !reflect.DeepEqual(document.Inputs, want) {
				t.Fatalf("inputs %+v, want %+v", document.Inputs, want)
			}
			for _, operation := range document.Operations {
				if !reflect.DeepEqual(operation.Security, om.Security{om.Bearer{Input: "OM_SIGN_IN"}}) {
					t.Fatalf("%s %s sends %+v, want the access token as a bearer token", operation.Method, operation.Path, operation.Security)
				}
			}
		})
	}
}

func TestResponses(t *testing.T) {
	const api = `openapi: 3.1.1
info: {title: Pets, version: '1'}
servers: [{url: 'https://pets.example.test'}]
paths:
  /pets:
    get:
      operationId: listPets
      responses:
        '200': {description: ok, content: {application/json: {schema: {type: array, items: {$ref: '#/components/schemas/Pet'}}}}}
        '404': {description: missing, content: {application/json: {schema: {$ref: '#/components/schemas/Error'}}}}
    head:
      operationId: checkPets
      responses:
        '200': {description: ok, content: {application/json: {schema: {type: array, items: {$ref: '#/components/schemas/Pet'}}}}}
    post:
      operationId: createPet
      responses:
        '200': {description: existing, content: {application/json: {schema: {$ref: '#/components/schemas/Pet'}}}}
        '201': {description: created, content: {application/json: {schema: {$ref: '#/components/schemas/Pet'}}}}
        '202': {description: queued, content: {application/json: {schema: {$ref: '#/components/schemas/Job'}}}}
  /pets/{id}:
    parameters: [{name: id, in: path, required: true, schema: {type: string}}]
    put:
      operationId: replacePet
      responses:
        '200': {description: replaced, content: {application/json: {schema: {$ref: '#/components/schemas/Pet'}}}}
        '204': {description: unchanged}
    delete:
      operationId: deletePet
      responses:
        '204': {description: deleted}
    patch:
      operationId: touchPet
      responses:
        '200': {description: anything, content: {application/json: {schema: true}}}
    get:
      operationId: getPetPhoto
      responses:
        '200': {description: photo, content: {image/png: {}}}
        '203': {description: cached, content: {application/json: {schema: {$ref: '#/components/schemas/Pet'}}}}
components:
  schemas:
    Pet: {type: object, required: [name, secret], properties: {name: {type: string}, secret: {type: string, writeOnly: true}, owner: {$ref: '#/components/schemas/Owner'}}}
    Owner: {type: object, properties: {name: {type: string}}}
    Job: {type: object, properties: {id: {type: integer}}}
    Error: {type: object, properties: {message: {type: string}}}
`
	compiled, err := compile.Compile(t.Context(), []byte(api), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, operation := range compiled.Document.Operations {
		if operation.OutputSchema == nil {
			continue
		}
		var schema any
		if err := json.Unmarshal(operation.OutputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		got[operation.Name] = schema
	}
	const pet = `"Pet": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "owner": {"$ref": "#/$defs/Owner"}}},
    "Owner": {"type": "object", "properties": {"name": {"type": "string"}}}`
	var want map[string]any
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{
  "listPets": {"type": "array", "items": {"$ref": "#/$defs/Pet"}, "$defs": {%[1]s}},
  "createPet": {"anyOf": [{"$ref": "#/$defs/Pet"}, {"$ref": "#/$defs/Job"}], "$defs": {%[1]s,
    "Job": {"type": "object", "properties": {"id": {"type": "integer"}}}}},
  "replacePet": {"anyOf": [{"$ref": "#/$defs/Pet"}, {"type": "null"}], "$defs": {%[1]s}}
}`, pet)), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("output schemas %v", got)
	}
}
