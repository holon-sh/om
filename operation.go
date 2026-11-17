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
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	InputSchema map[string]any `json:"inputSchema"`
	Parameters  []parameter    `json:"parameters"`
	Body        bool           `json:"body"`
	Security    []credentials  `json:"security"`
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
		return operation{}, nil, fmt.Errorf("HTTP method %s is unsupported", method)
	}
	if len(item.Servers) != 0 || op.Servers != nil && len(*op.Servers) != 0 {
		return operation{}, nil, errors.New("path- and operation-level servers are unsupported")
	}
	auth, inputs, err := compileSecurity(doc, op)
	if err != nil {
		return operation{}, nil, err
	}
	tool := operation{
		Name: toolName(op, method, path), Title: op.Summary, Description: op.Description, Method: method, Path: path,
		Parameters: []parameter{}, Security: auth,
	}
	builder := schemaBuilder{doc: doc, definitions: map[string]any{}}
	properties := map[string]any{}
	var required []string
	locations := map[string]string{}
	for _, param := range effectiveParameters(item, op) {
		if ignoreHeader(param) {
			continue
		}
		if in, taken := locations[param.Name]; taken {
			return operation{}, nil, fmt.Errorf("%s and %s parameters share the name %q", in, param.In, param.Name)
		}
		locations[param.Name] = param.In
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
		properties[param.Name] = described(shape, param.Description)
		if param.Required {
			required = append(required, param.Name)
		}
		tool.Parameters = append(tool.Parameters, parameter{param.Name, param.In, serialization.Style, serialization.Explode})
	}
	if op.RequestBody != nil {
		body := op.RequestBody.Value
		content := body.Content["application/json"]
		switch {
		case content == nil:
			return operation{}, nil, fmt.Errorf("request body media type %s is unsupported (only application/json is)", strings.Join(slices.Sorted(maps.Keys(body.Content)), ", "))
		case method == http.MethodGet || method == http.MethodHead:
			return operation{}, nil, fmt.Errorf("request bodies on %s are unsupported", method)
		case locations["body"] != "":
			return operation{}, nil, fmt.Errorf("%s parameter %q conflicts with the request body argument", locations["body"], "body")
		}
		shape, err := builder.convert(content.Schema)
		if err != nil {
			return operation{}, nil, fmt.Errorf("request body: %w", err)
		}
		properties["body"] = described(shape, body.Description)
		if body.Required {
			required = append(required, "body")
		}
		tool.Body = true
	}
	tool.InputSchema = map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) != 0 {
		tool.InputSchema["required"] = required
	}
	if len(builder.definitions) != 0 {
		tool.InputSchema["$defs"] = builder.definitions
	}
	return tool, inputs, nil
}

func toolName(op *openapi3.Operation, method, path string) string {
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
	return name
}

func described(shape any, description string) any {
	fields, ok := shape.(map[string]any)
	if ok && description != "" {
		fields["description"] = description
	}
	return shape
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
	if len(p.Content) != 0 {
		return fmt.Errorf("%s parameter %q is described by content, which is unsupported", p.In, p.Name)
	}
	if p.In == openapi3.ParameterInHeader && prohibitedHeader(p.Name) {
		return fmt.Errorf("header parameter %q cannot be sent", p.Name)
	}
	style := serialization.Style
	switch p.In {
	case openapi3.ParameterInHeader, openapi3.ParameterInPath:
		if style == openapi3.SerializationSimple {
			return nil
		}
	case openapi3.ParameterInQuery:
		if style == openapi3.SerializationForm || style == openapi3.SerializationDeepObject && serialization.Explode {
			return nil
		}
	case openapi3.ParameterInCookie:
		if style == openapi3.SerializationForm {
			return nil
		}
	}
	return fmt.Errorf("%s parameter %q uses style %s, which is unsupported", p.In, p.Name, style)
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
	return nil, nil, errors.New("no security requirement uses only API key, HTTP bearer or HTTP basic schemes")
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
