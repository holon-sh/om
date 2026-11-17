package om

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

type Options struct {
	AppName string
}

type Project struct {
	Files       map[string][]byte
	Name        string
	Title       string
	Version     string
	Description string
	Inputs      []Input
	Endpoint    Endpoint
	Omitted     []Omission
}

type Input struct {
	Name        string
	Description string
	Default     *string
	Choices     []string
	Secret      bool
}

type Endpoint struct {
	Host string
	Port int
}

type Omission struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func (o Omission) String() string {
	return o.Method + " " + o.Path + ": " + o.Reason
}

var appName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Generate turns one OpenAPI document into a self-contained TypeScript project.
// The caller supplies the bytes and, for relative server URLs, their HTTPS location.
func Generate(ctx context.Context, data []byte, location *url.URL, options Options) (Project, error) {
	if !appName.MatchString(options.AppName) {
		return Project{}, errors.New("app name must be a lowercase DNS label of at most 63 characters")
	}
	loader := openapi3.NewLoader()
	loader.Context = ctx
	var doc *openapi3.T
	var err error
	if location == nil {
		doc, err = loader.LoadFromData(data)
	} else {
		doc, err = loader.LoadFromDataWithPath(data, location)
	}
	if err != nil {
		return Project{}, fmt.Errorf("load OpenAPI document: %w", err)
	}
	switch doc.OpenAPIMajorMinor() {
	case "3.0", "3.1", "3.2":
	default:
		return Project{}, errors.New("expected an OpenAPI 3.0, 3.1, or 3.2 document")
	}
	if err := doc.Validate(ctx, openapi3.DisableExamplesValidation()); err != nil {
		return Project{}, fmt.Errorf("validate OpenAPI document: %w", err)
	}
	server, err := resolveServer(doc.Servers, location)
	if err != nil {
		return Project{}, err
	}

	model := generatedDocument{
		Name: options.AppName, Version: doc.Info.Version,
		Server: server.url, Variables: server.variables, Operations: []operation{},
	}
	inputs := server.inputs
	paths := doc.Paths.Map()
	names := map[string]struct{}{}
	var omitted []Omission
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		item := paths[path]
		methods := item.Operations()
		for _, method := range slices.Sorted(maps.Keys(methods)) {
			tool, required, err := compileOperation(doc, item, methods[method], method, path)
			if err != nil {
				omitted = append(omitted, Omission{method, path, err.Error()})
				continue
			}
			if _, exists := names[tool.Name]; exists {
				return Project{}, fmt.Errorf("multiple operations map to tool name %q", tool.Name)
			}
			names[tool.Name] = struct{}{}
			for _, input := range required {
				if err := addInput(inputs, input); err != nil {
					return Project{}, err
				}
			}
			model.Operations = append(model.Operations, tool)
		}
	}
	if len(model.Operations) == 0 {
		err := errors.New("document has no supported operations")
		if len(omitted) != 0 {
			err = fmt.Errorf("%w (first omission: %s)", err, omitted[0])
		}
		return Project{}, err
	}
	slices.SortFunc(model.Operations, func(a, b operation) int { return strings.Compare(a.Name, b.Name) })
	files, err := render(model)
	if err != nil {
		return Project{}, err
	}
	project := Project{
		Files: files, Name: model.Name, Title: doc.Info.Title, Version: model.Version,
		Description: doc.Info.Description, Endpoint: server.endpoint, Omitted: omitted,
	}
	for _, name := range slices.Sorted(maps.Keys(inputs)) {
		project.Inputs = append(project.Inputs, inputs[name].Input)
	}
	return project, nil
}

type generatedDocument struct {
	Name       string      `json:"name"`
	Version    string      `json:"version"`
	Server     string      `json:"server"`
	Variables  []variable  `json:"variables"`
	Operations []operation `json:"operations"`
}

type input struct {
	Input
	source string
}

func addInput(inputs map[string]input, value input) error {
	if len(value.Name) > 100 {
		return fmt.Errorf("input %q maps to invalid environment variable %q", value.source, value.Name)
	}
	if previous, exists := inputs[value.Name]; exists && previous.source != value.source {
		return fmt.Errorf("inputs %q and %q map to %s", previous.source, value.source, value.Name)
	}
	inputs[value.Name] = value
	return nil
}

var envChars = regexp.MustCompile(`[^A-Z0-9]`)

func envName(name string) string {
	return "OM_" + envChars.ReplaceAllString(strings.ToUpper(name), "_")
}

//go:embed internal/emit/server.ts.tmpl
var serverTemplate string

//go:embed internal/emit/package-lock.json
var npmLock string

var protoKey = regexp.MustCompile(`(?m)^([ \t]+)"__proto__":`)

func render(model generatedDocument) (map[string][]byte, error) {
	document, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		return nil, err
	}
	source := strings.Replace(serverTemplate, "/*OM_DOCUMENT*/", string(protoKey.ReplaceAll(document, []byte(`${1}["__proto__"]:`))), 1)
	pkg := strings.ReplaceAll(`{
  "name": "om-generated-app",
  "private": true,
  "type": "module",
  "scripts": {
    "start": "node server.ts"
  },
  "engines": {
    "node": ">=22.18.0"
  },
  "dependencies": {
    "@modelcontextprotocol/node": "2.1.2",
    "@modelcontextprotocol/server": "2.4.0"
  }
}
`, "om-generated-app", model.Name)
	return map[string][]byte{
		"server.ts":         []byte(source),
		"package.json":      []byte(pkg),
		"package-lock.json": []byte(strings.ReplaceAll(npmLock, `"name": "om-generated-app"`, `"name": "`+model.Name+`"`)),
	}, nil
}
