package om

import (
	"bytes"
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

type Project struct {
	Files       map[string][]byte
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

// Generate turns one OpenAPI document into a self-contained TypeScript project.
// The caller supplies the bytes and, for relative server URLs, their HTTPS location.
func Generate(ctx context.Context, data []byte, location *url.URL) (Project, error) {
	doc, err := load(ctx, data)
	if err != nil {
		return Project{}, fmt.Errorf("load OpenAPI document: %w", err)
	}
	switch doc.OpenAPIMajorMinor() {
	case "3.0", "3.1", "3.2":
	default:
		return Project{}, errors.New("expected an OpenAPI 3.0, 3.1, or 3.2 document")
	}
	if doc.Info == nil {
		return Project{}, errors.New("OpenAPI document has no info object")
	}
	server, err := resolveServer(doc.Servers, location)
	if err != nil {
		return Project{}, err
	}

	model := generatedDocument{
		Title: doc.Info.Title, Version: doc.Info.Version,
		Server: server.url, Variables: server.variables, Operations: []operation{},
	}
	inputs := server.inputs
	owners := map[string]string{}
	var omitted []Omission
	paths := doc.Paths.Map()
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		item := paths[path]
		methods := item.Operations()
		for _, method := range slices.Sorted(maps.Keys(methods)) {
			tool, needed, err := compileOperation(doc, item, methods[method], method, path)
			if owner, taken := owners[tool.Name]; err == nil && taken {
				err = fmt.Errorf("tool name %q is already used by %s", tool.Name, owner)
			}
			if err == nil {
				err = addInputs(inputs, needed...)
			}
			if err != nil {
				omitted = append(omitted, Omission{method, path, err.Error()})
				continue
			}
			owners[tool.Name] = method + " " + path
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
	var document bytes.Buffer
	encoder := json.NewEncoder(&document)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(model); err != nil {
		return Project{}, err
	}
	project := Project{
		Files: map[string][]byte{
			"document.json":     document.Bytes(),
			"server.ts":         []byte(serverSource),
			"package.json":      []byte(packageManifest),
			"package-lock.json": []byte(packageLock),
		},
		Title: doc.Info.Title, Version: doc.Info.Version, Description: doc.Info.Description,
		Endpoint: server.endpoint, Omitted: omitted,
	}
	for _, name := range slices.Sorted(maps.Keys(inputs)) {
		project.Inputs = append(project.Inputs, inputs[name].Input)
	}
	return project, nil
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

type generatedDocument struct {
	Title      string      `json:"title"`
	Version    string      `json:"version"`
	Server     string      `json:"server"`
	Variables  []variable  `json:"variables"`
	Operations []operation `json:"operations"`
}

type input struct {
	Input
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
		claimed[value.Name] = value
	}
	maps.Copy(inputs, claimed)
	return nil
}

var envChars = regexp.MustCompile(`[^A-Z0-9]`)

func envName(name string) string {
	return "OM_" + envChars.ReplaceAllString(strings.ToUpper(name), "_")
}

//go:embed internal/emit/server.ts
var serverSource string

//go:embed internal/emit/package.json
var packageManifest string

//go:embed internal/emit/package-lock.json
var packageLock string
