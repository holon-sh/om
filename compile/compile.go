package compile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/holon-sh/om"
	"github.com/holon-sh/om/internal/schema"
)

type Omission struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func (o Omission) String() string {
	return o.Method + " " + o.Path + ": " + o.Reason
}

// Compiled is what one OpenAPI document compiles to.
type Compiled struct {
	// Document is the model a host stores and serves.
	Document om.Document
	// Omitted are the operations Document leaves out.
	Omitted []Omission
}

// Compile turns one OpenAPI 3.0–3.2 document into a Document and the operations it left out.
// location resolves relative server URLs.
func Compile(ctx context.Context, data []byte, location *url.URL) (Compiled, error) {
	doc, err := load(ctx, data)
	if err != nil {
		return Compiled{}, fmt.Errorf("load OpenAPI document: %w", err)
	}
	switch doc.OpenAPIMajorMinor() {
	case "3.0", "3.1", "3.2":
	default:
		return Compiled{}, errors.New("expected an OpenAPI 3.0, 3.1, or 3.2 document")
	}
	if doc.Info == nil {
		return Compiled{}, errors.New("OpenAPI document has no info object")
	}
	server, inputs, err := resolveServer(doc.Servers, location)
	if err != nil {
		return Compiled{}, err
	}

	tools := map[string]om.Operation{}
	builder := schemaBuilder{doc: doc, definitions: map[string]any{}}
	definitions := map[string]json.RawMessage{}
	compiler := schema.Compiler{}
	var omitted []Omission
	paths := doc.Paths.Map()
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		item := paths[path]
		methods := item.Operations()
		for _, method := range slices.Sorted(maps.Keys(methods)) {
			tool, needed, err := compileOperation(builder, item, methods[method], method, path)
			if err == nil {
				err = check(compiler, builder, definitions, tool)
			}
			if existing, taken := tools[tool.Name]; err == nil && taken {
				err = fmt.Errorf("tool name %q is already used by %s %s", tool.Name, existing.Method, existing.Path)
			}
			if err == nil {
				err = addInputs(inputs, needed...)
			}
			if err != nil {
				omitted = append(omitted, Omission{method, path, err.Error()})
				continue
			}
			tools[tool.Name] = tool
		}
	}
	if len(tools) == 0 {
		err := errors.New("document has no supported operations")
		if len(omitted) != 0 {
			err = fmt.Errorf("%w (first omission: %s)", err, omitted[0])
		}
		return Compiled{}, err
	}
	document := om.Document{
		Format: om.Format, Title: doc.Info.Title, Version: doc.Info.Version, Description: doc.Info.Description,
		Server: server, Inputs: []om.Input{},
	}
	for _, name := range slices.Sorted(maps.Keys(inputs)) {
		document.Inputs = append(document.Inputs, inputs[name].Input)
	}
	var roots []any
	for _, name := range slices.Sorted(maps.Keys(tools)) {
		operation := tools[name]
		document.Operations = append(document.Operations, operation)
		var input, output any
		json.Unmarshal(operation.InputSchema, &input)
		json.Unmarshal(operation.OutputSchema, &output)
		roots = append(roots, input, output)
	}
	reached, _ := schema.Reach(func(name string) (any, error) { return builder.definitions[name], nil }, roots...)
	if len(reached) != 0 {
		document.Definitions = make(map[string]json.RawMessage, len(reached))
	}
	for name, definition := range reached {
		if document.Definitions[name], err = json.Marshal(definition); err != nil {
			return Compiled{}, err
		}
	}
	return Compiled{Document: document, Omitted: omitted}, nil
}

func load(ctx context.Context, data []byte) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.Context = ctx
	if !json.Valid(data) {
		return loader.LoadFromData(data)
	}
	doc := &openapi3.T{}
	if err := json.Unmarshal(data, doc); err != nil {
		return nil, err
	}
	return doc, loader.ResolveRefsIn(doc, nil)
}

type input struct {
	om.Input
	source string
}

func addInputs(inputs map[string]input, values ...input) error {
	claimed := map[string]input{}
	for _, value := range values {
		previous, exists := inputs[value.Name]
		if !exists {
			previous, exists = claimed[value.Name]
		}
		if exists && previous.source != value.source {
			return fmt.Errorf("%s and %s both map to environment variable %s", previous.source, value.source, value.Name)
		}
		if exists && value.Authorization != nil {
			value.Authorization = &om.Authorization{
				Issuer: value.Authorization.Issuer, Scopes: union(previous.Authorization.Scopes, value.Authorization.Scopes),
			}
		}
		claimed[value.Name] = value
	}
	maps.Copy(inputs, claimed)
	return nil
}

var envChars = regexp.MustCompile(`[^A-Z0-9]`)

func envName(name string) string {
	return "OM_" + envChars.ReplaceAllString(strings.ToUpper(name), "_")
}
