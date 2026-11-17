package om

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

type operation struct {
	Name        string        `json:"name"`
	Title       string        `json:"title,omitempty"`
	Description string        `json:"description,omitempty"`
	Method      string        `json:"method"`
	Path        string        `json:"path"`
	InputSchema schema        `json:"inputSchema"`
	Parameters  []parameter   `json:"parameters"`
	Security    []credentials `json:"security"`
}

type parameter struct {
	Name    string `json:"name"`
	In      string `json:"in"`
	Style   string `json:"style"`
	Explode bool   `json:"explode"`
}

var toolNameChar = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func compileOperation(doc *openapi3.T, item *openapi3.PathItem, op *openapi3.Operation, method, path string) (operation, []input, error) {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodPost, http.MethodPatch:
	default:
		return operation{}, nil, errors.New("HTTP method is unsupported by generated apps")
	}
	if len(item.Servers) != 0 || op.Servers != nil && len(*op.Servers) != 0 {
		return operation{}, nil, errors.New("path- and operation-level servers are unsupported")
	}
	name := op.OperationID
	if name == "" {
		name = strings.ToLower(method) + "_" + strings.TrimPrefix(path, "/")
	}
	name = toolNameChar.ReplaceAllString(name, "_")
	name = name[:min(len(name), 128)]
	if slices.Contains([]string{
		"constructor", "__defineGetter__", "__defineSetter__", "hasOwnProperty", "__lookupGetter__",
		"__lookupSetter__", "isPrototypeOf", "propertyIsEnumerable", "toString", "valueOf", "__proto__", "toLocaleString",
	}, name) {
		name = "_" + name
	}
	auth, inputs, err := compileSecurity(doc, op)
	if err != nil {
		return operation{}, nil, err
	}
	tool := operation{
		Name: name, Title: op.Summary, Description: op.Description, Method: method, Path: path,
		Parameters: []parameter{}, Security: auth,
	}
	builder := schemaBuilder{doc: doc, definitions: map[string]any{}}
	groups := map[string]objectSchema{}
	for _, param := range effectiveParameters(item, op) {
		if ignoreHeader(param) {
			continue
		}
		serialization, err := param.SerializationMethod()
		if err != nil {
			return operation{}, nil, err
		}
		if err := checkParameter(param, serialization); err != nil {
			return operation{}, nil, err
		}
		shape, err := builder.convert(param.Schema)
		if err != nil {
			return operation{}, nil, fmt.Errorf("parameter %q: %w", param.Name, err)
		}
		group := groups[param.In]
		group.add(param.Name, shape, param.Required)
		groups[param.In] = group
		tool.Parameters = append(tool.Parameters, parameter{param.Name, param.In, serialization.Style, serialization.Explode})
	}
	root := objectSchema{}
	for _, key := range slices.Sorted(maps.Keys(groups)) {
		group := groups[key]
		root.add(key, group.schema(), len(group.required) != 0)
	}
	if op.RequestBody != nil {
		content := op.RequestBody.Value.Content["application/json"]
		if content == nil || method == http.MethodGet || method == http.MethodHead {
			return operation{}, nil, errors.New("request body is unsupported for this media type or HTTP method")
		}
		shape, err := builder.convert(content.Schema)
		if err != nil {
			return operation{}, nil, fmt.Errorf("request body: %w", err)
		}
		root.add("body", shape, op.RequestBody.Value.Required)
	}
	tool.InputSchema = root.schema()
	if len(builder.definitions) != 0 {
		tool.InputSchema["$defs"] = builder.definitions
	}
	return tool, inputs, nil
}

func effectiveParameters(item *openapi3.PathItem, op *openapi3.Operation) []*openapi3.Parameter {
	type key struct{ in, name string }
	effective := map[key]*openapi3.Parameter{}
	for _, params := range []openapi3.Parameters{item.Parameters, op.Parameters} {
		for _, ref := range params {
			p := ref.Value
			name := p.Name
			if p.In == openapi3.ParameterInHeader {
				name = strings.ToLower(name)
			}
			effective[key{p.In, name}] = p
		}
	}
	return slices.SortedFunc(maps.Values(effective), func(a, b *openapi3.Parameter) int {
		if order := strings.Compare(a.In, b.In); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
}

func ignoreHeader(p *openapi3.Parameter) bool {
	return p.In == openapi3.ParameterInHeader &&
		slices.Contains([]string{"accept", "content-type", "authorization"}, strings.ToLower(p.Name))
}

func checkParameter(p *openapi3.Parameter, serialization *openapi3.SerializationMethod) error {
	switch {
	case len(p.Content) != 0, p.AllowReserved, p.AllowEmptyValue:
	case p.In == openapi3.ParameterInHeader && prohibitedHeader(p.Name):
	case p.In == openapi3.ParameterInHeader || p.In == openapi3.ParameterInPath:
		if serialization.Style == openapi3.SerializationSimple {
			return nil
		}
	case serialization.Style == openapi3.SerializationForm ||
		p.In == openapi3.ParameterInQuery && serialization.Style == openapi3.SerializationDeepObject && serialization.Explode:
		return nil
	}
	return fmt.Errorf("parameter %q uses an unsupported HTTP encoding", p.Name)
}

type credentialDestination struct {
	location string
	name     string
}

type credentials interface {
	destination() credentialDestination
}

type apiKey struct {
	Kind string `json:"kind"`
	In   string `json:"in"`
	Name string `json:"name"`
	Env  string `json:"env"`
}

type bearer struct {
	Kind string `json:"kind"`
	Env  string `json:"env"`
}

type basic struct {
	Kind        string `json:"kind"`
	UsernameEnv string `json:"usernameEnv"`
	PasswordEnv string `json:"passwordEnv"`
}

func (k apiKey) destination() credentialDestination {
	name := k.Name
	if k.In == "header" {
		name = strings.ToLower(name)
	}
	return credentialDestination{k.In, name}
}

func (bearer) destination() credentialDestination {
	return credentialDestination{"header", "authorization"}
}
func (basic) destination() credentialDestination {
	return credentialDestination{"header", "authorization"}
}

func compileSecurity(doc *openapi3.T, op *openapi3.Operation) ([]credentials, []input, error) {
	requirements := doc.Security
	if op.Security != nil {
		requirements = *op.Security
	}
	if len(requirements) == 0 {
		return []credentials{}, nil, nil
	}
	for _, requirement := range requirements {
		auth := make([]credentials, 0, len(requirement))
		var inputs []input
		destinations := map[credentialDestination]struct{}{}
		for _, name := range slices.Sorted(maps.Keys(requirement)) {
			if doc.Components == nil {
				break
			}
			ref := doc.Components.SecuritySchemes[name]
			if ref == nil {
				break
			}
			value, needed := compileScheme(name, ref.Value)
			if value == nil {
				break
			}
			destination := value.destination()
			if _, exists := destinations[destination]; exists {
				break
			}
			destinations[destination] = struct{}{}
			auth = append(auth, value)
			inputs = append(inputs, needed...)
		}
		if len(auth) == len(requirement) {
			return auth, inputs, nil
		}
	}
	return nil, nil, errors.New("no supported security requirement (expected API key, bearer or basic)")
}

func compileScheme(name string, scheme *openapi3.SecurityScheme) (credentials, []input) {
	secret := func(suffix string) input {
		return input{
			Input:  Input{Name: envName(name) + suffix, Description: scheme.Description, Secret: true},
			source: "security scheme " + name + suffix,
		}
	}
	switch {
	case scheme.Type == "apiKey":
		switch scheme.In {
		case "header":
			if prohibitedHeader(scheme.Name) {
				return nil, nil
			}
		case "query":
		case "cookie":
			if !httpToken.MatchString(scheme.Name) {
				return nil, nil
			}
		default:
			return nil, nil
		}
		key := secret("")
		return apiKey{"apiKey", scheme.In, scheme.Name, key.Name}, []input{key}
	case scheme.Type == "http" && strings.EqualFold(scheme.Scheme, "bearer"):
		token := secret("")
		return bearer{"bearer", token.Name}, []input{token}
	case scheme.Type == "http" && strings.EqualFold(scheme.Scheme, "basic"):
		user, password := secret("_USERNAME"), secret("_PASSWORD")
		return basic{"basic", user.Name, password.Name}, []input{user, password}
	}
	return nil, nil
}

var httpToken = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func prohibitedHeader(name string) bool {
	return !httpToken.MatchString(name) || slices.Contains([]string{
		"connection", "content-length", "cookie", "host", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade",
	}, strings.ToLower(name))
}

type objectSchema struct {
	properties map[string]any
	required   []string
}

func (o *objectSchema) add(name string, shape any, required bool) {
	if o.properties == nil {
		o.properties = map[string]any{}
	}
	o.properties[name] = shape
	if required {
		o.required = append(o.required, name)
	}
}

func (o objectSchema) schema() schema {
	value := schema{"type": "object", "additionalProperties": false}
	if len(o.properties) != 0 {
		value["properties"] = o.properties
	}
	if len(o.required) != 0 {
		value["required"] = o.required
	}
	return value
}
