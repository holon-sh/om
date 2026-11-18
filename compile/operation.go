package compile

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/holon-sh/om"
)

var toolNameChar = regexp.MustCompile(`[^A-Za-z0-9_.-]|^[.-]|[.-]$`)

func compileOperation(doc *openapi3.T, item *openapi3.PathItem, op *openapi3.Operation, method, path string) (om.Operation, []input, error) {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodPost, http.MethodPatch:
	default:
		return om.Operation{}, nil, fmt.Errorf("HTTP method %s is unsupported", method)
	}
	if len(item.Servers) != 0 || op.Servers != nil && len(*op.Servers) != 0 {
		return om.Operation{}, nil, errors.New("path- and operation-level servers are unsupported")
	}
	auth, inputs, err := compileSecurity(doc, op)
	if err != nil {
		return om.Operation{}, nil, err
	}
	tool := om.Operation{
		Name: toolName(op, method, path), Title: op.Summary, Description: op.Description, Method: method, Path: path,
		Security: auth,
	}
	builder := schemaBuilder{doc: doc, definitions: map[string]any{}}
	properties := map[string]any{}
	var required []string
	argument := func(name string, shape any, description string, isRequired bool) error {
		if _, taken := properties[name]; taken {
			return fmt.Errorf("more than one parameter or request body maps to the tool argument %q", name)
		}
		if fields, ok := shape.(map[string]any); ok && description != "" {
			fields["description"] = description
		}
		properties[name] = shape
		if isRequired {
			required = append(required, name)
		}
		return nil
	}
	for _, param := range effectiveParameters(item, op) {
		if ignoreHeader(param) {
			continue
		}
		serialization, err := param.SerializationMethod()
		if err != nil {
			return om.Operation{}, nil, err
		}
		if err := checkParameter(param, serialization); err != nil {
			return om.Operation{}, nil, err
		}
		shape, err := builder.convert(param.Schema)
		if err != nil {
			return om.Operation{}, nil, fmt.Errorf("parameter %q: %w", param.Name, err)
		}
		if err := argument(param.Name, shape, param.Description, param.Required); err != nil {
			return om.Operation{}, nil, err
		}
		in := om.Location(param.In)
		if serialization.Style == openapi3.SerializationDeepObject {
			in = om.DeepObject
		}
		tool.Parameters = append(tool.Parameters, om.Parameter{Name: param.Name, In: in, Explode: serialization.Explode})
	}
	if op.RequestBody != nil {
		body := op.RequestBody.Value
		content := body.Content["application/json"]
		switch {
		case content == nil:
			return om.Operation{}, nil, fmt.Errorf("request body media type %s is unsupported (only application/json is)", strings.Join(slices.Sorted(maps.Keys(body.Content)), ", "))
		case method == http.MethodGet || method == http.MethodHead:
			return om.Operation{}, nil, fmt.Errorf("request bodies on %s are unsupported", method)
		}
		shape, err := builder.convert(content.Schema)
		if err != nil {
			return om.Operation{}, nil, fmt.Errorf("request body: %w", err)
		}
		if err := argument("body", shape, body.Description, body.Required); err != nil {
			return om.Operation{}, nil, err
		}
		tool.Body = true
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) != 0 {
		schema["required"] = required
	}
	if len(builder.definitions) != 0 {
		schema["$defs"] = builder.definitions
	}
	tool.InputSchema, err = json.Marshal(schema)
	return tool, inputs, err
}

// toolName follows the MCP tool name format: https://github.com/modelcontextprotocol/modelcontextprotocol/issues/986
func toolName(op *openapi3.Operation, method, path string) string {
	name := op.OperationID
	if name == "" {
		name = strings.ToLower(method) + "_" + strings.TrimPrefix(path, "/")
	}
	name = toolNameChar.ReplaceAllString(name[:min(len(name), 128)], "_")
	if slices.Contains([]string{
		"constructor", "__defineGetter__", "__defineSetter__", "hasOwnProperty", "__lookupGetter__",
		"__lookupSetter__", "isPrototypeOf", "propertyIsEnumerable", "toString", "valueOf", "__proto__", "toLocaleString",
	}, name) {
		name = "_" + name
	}
	return name
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

func destination(credential om.Credential) string {
	key, ok := credential.(om.APIKey)
	switch {
	case !ok:
		return "header authorization"
	case key.In == om.Header:
		return "header " + strings.ToLower(key.Name)
	}
	return string(key.In) + " " + key.Name
}

func compileSecurity(doc *openapi3.T, op *openapi3.Operation) (om.Security, []input, error) {
	requirements := doc.Security
	if op.Security != nil {
		requirements = *op.Security
	}
	if len(requirements) == 0 {
		return nil, nil, nil
	}
	for _, requirement := range requirements {
		auth := make(om.Security, 0, len(requirement))
		var inputs []input
		for _, name := range slices.Sorted(maps.Keys(requirement)) {
			if doc.Components == nil {
				break
			}
			ref := doc.Components.SecuritySchemes[name]
			if ref == nil {
				break
			}
			value, needed := compileScheme(name, ref.Value, requirement[name])
			if value == nil || slices.ContainsFunc(auth, func(c om.Credential) bool { return destination(c) == destination(value) }) {
				break
			}
			auth = append(auth, value)
			inputs = append(inputs, needed...)
		}
		if len(auth) == len(requirement) {
			return auth, inputs, nil
		}
	}
	return nil, nil, errors.New("no security requirement uses only API key, HTTP bearer, HTTP basic, OAuth 2.0 or OpenID Connect schemes")
}

// compileScheme returns the credential an operation sends for a scheme and the inputs that supply it. An [OAuth 2.0]
// scheme names its authorization server by oauth2MetadataUrl and an [OpenID Connect] scheme by openIdConnectUrl, and
// scopes are the ones the operation's [security requirement] names.
//
// [OAuth 2.0]: https://spec.openapis.org/oas/v3.2.0.html#security-scheme-oauth2-metadata-url
// [OpenID Connect]: https://spec.openapis.org/oas/v3.2.0.html#security-scheme-open-id-connect-url
// [security requirement]: https://spec.openapis.org/oas/v3.2.0.html#security-requirement-object
func compileScheme(name string, scheme *openapi3.SecurityScheme, scopes []string) (om.Credential, []input) {
	secret := func(suffix string) input {
		return input{
			Input:  om.Input{Name: envName(name) + suffix},
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
		return om.APIKey{In: om.Location(scheme.In), Name: scheme.Name, Input: key.Name}, []input{key}
	case scheme.Type == "http" && strings.EqualFold(scheme.Scheme, "bearer"):
		token := secret("")
		return om.Bearer{Input: token.Name}, []input{token}
	case scheme.Type == "http" && strings.EqualFold(scheme.Scheme, "basic"):
		user, password := secret("_USERNAME"), secret("_PASSWORD")
		return om.Basic{Username: user.Name, Password: password.Name}, []input{user, password}
	case scheme.Type == "oauth2" || scheme.Type == "openIdConnect":
		metadata, _ := scheme.Extensions["oauth2MetadataUrl"].(string)
		server, ok := issuer(cmp.Or(scheme.OpenIdConnectUrl, metadata))
		if !ok {
			return nil, nil
		}
		token := secret("")
		token.Authorization = &om.Authorization{Issuer: server, Scopes: union(scopes)}
		return om.Bearer{Input: token.Name}, []input{token}
	}
	return nil, nil
}

// issuer is the issuer identifier of the authorization server whose metadata is at location: [RFC 8414 §3.1] inserts
// /.well-known/oauth-authorization-server between the issuer's host and path, and [OpenID Connect Discovery 1.0 §4]
// appends /.well-known/openid-configuration to the issuer.
//
// [RFC 8414 §3.1]: https://www.rfc-editor.org/rfc/rfc8414.html#section-3.1
// [OpenID Connect Discovery 1.0 §4]: https://openid.net/specs/openid-connect-discovery-1_0.html#ProviderConfig
func issuer(location string) (string, bool) {
	address, err := url.Parse(location)
	if err != nil {
		return "", false
	}
	origin, path := address.Scheme+"://"+address.Host, address.EscapedPath()
	if rest, inserted := strings.CutPrefix(path, "/.well-known/oauth-authorization-server"); inserted && (rest == "" || rest[0] == '/') {
		return origin + rest, true
	}
	rest, appended := strings.CutSuffix(path, "/.well-known/openid-configuration")
	return origin + rest, appended
}

func union(sets ...[]string) []string {
	return slices.Compact(slices.Sorted(slices.Values(slices.Concat(sets...))))
}

var httpToken = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func prohibitedHeader(name string) bool {
	return !httpToken.MatchString(name) || slices.Contains([]string{
		"connection", "content-length", "cookie", "host", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade",
	}, strings.ToLower(name))
}
