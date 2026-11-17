package om

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type refusal string

func (r refusal) Error() string { return string(r) }

func (d Document) call(ctx context.Context, o Operation, arguments json.RawMessage, lookup func(string) (string, bool), client *http.Client) *mcp.CallToolResult {
	if arguments == nil {
		arguments = json.RawMessage("{}")
	}
	values, err := validate(o.InputSchema, arguments)
	if err != nil {
		return failure(fmt.Sprintf("Input validation error: Invalid arguments for tool %s: %v", o.Name, err))
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	text, err := d.send(ctx, o, values, lookup, client)
	var refused refusal
	switch {
	case errors.As(err, &refused):
		return failure(refused.Error())
	case err != nil:
		return failure("Could not complete the upstream request.")
	}
	return &mcp.CallToolResult{Content: text}
}

func failure(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}

func validate(schema, arguments json.RawMessage) (map[string]any, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseRegexpEngine(func(pattern string) (jsonschema.Regexp, error) {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return unenforced(pattern), nil
		}
		return compiled, nil
	})
	const location = "urn:om:input"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(arguments))
	if err != nil {
		return nil, err
	}
	if err := compiled.Validate(value); err != nil {
		return nil, err
	}
	return value.(map[string]any), nil
}

type unenforced string

func (p unenforced) String() string        { return string(p) }
func (unenforced) MatchString(string) bool { return true }

func (d Document) send(ctx context.Context, o Operation, values map[string]any, lookup func(string) (string, bool), client *http.Client) ([]mcp.Content, error) {
	request, err := d.request(ctx, o, values, lookup)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	text, err := responseText(response)
	switch {
	case err != nil:
		return nil, err
	case response.StatusCode/100 != 2 && text != "":
		return nil, refusal(fmt.Sprintf("HTTP %d: %s", response.StatusCode, text))
	case response.StatusCode/100 != 2:
		return nil, refusal(fmt.Sprintf("HTTP %d", response.StatusCode))
	case text == "":
		return []mcp.Content{}, nil
	}
	return []mcp.Content{&mcp.TextContent{Text: text}}, nil
}

func responseText(response *http.Response) (string, error) {
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	switch {
	case err != nil:
		return "", err
	case len(data) > limit:
		return "", refusal("The upstream response exceeds 1 MiB.")
	case !utf8.Valid(data) && response.Header.Get("Content-Type") != "":
		return "", refusal(fmt.Sprintf("The upstream response (%s) is not UTF-8.", response.Header.Get("Content-Type")))
	case !utf8.Valid(data):
		return "", refusal("The upstream response is not UTF-8.")
	}
	return string(data), nil
}

type upstream struct {
	path    map[string]string
	header  http.Header
	query   []pair
	cookies []pair
}

type pair struct{ name, value string }

var placeholder = regexp.MustCompile(`\{([^{}]+)\}`)
var dotSegment = regexp.MustCompile(`(?i)^(?:\.|%2e){1,2}$`)

func (d Document) request(ctx context.Context, o Operation, values map[string]any, lookup func(string) (string, bool)) (*http.Request, error) {
	server, err := d.serverURL(lookup)
	if err != nil {
		return nil, err
	}
	r := upstream{path: map[string]string{}, header: http.Header{}}
	for _, parameter := range o.Parameters {
		escape := encode
		if parameter.In == Header {
			escape = func(value string) string { return value }
		}
		value, err := argumentOf(values[parameter.Name], escape)
		if err != nil {
			return nil, err
		}
		if len(value.parts) == 0 {
			continue
		}
		name := encode(parameter.Name)
		switch parameter.In {
		case Path:
			r.path[parameter.Name] = value.simple(parameter.Explode)
		case Header:
			r.header.Set(parameter.Name, value.simple(parameter.Explode))
		case Query:
			r.query = append(r.query, value.form(name, parameter.Explode)...)
		case Cookie:
			r.cookies = append(r.cookies, value.form(name, parameter.Explode)...)
		case DeepObject:
			if !value.object {
				return nil, refusal("A deepObject parameter must be an object.")
			}
			for i := 0; i < len(value.parts); i += 2 {
				r.query = append(r.query, pair{name + "%5B" + value.parts[i] + "%5D", value.parts[i+1]})
			}
		}
	}
	for _, credential := range o.Security {
		if err := credential.attach(&r, lookup); err != nil {
			return nil, err
		}
	}
	var missing error
	path := placeholder.ReplaceAllStringFunc(o.Path, func(field string) string {
		value, ok := r.path[field[1:len(field)-1]]
		if !ok && missing == nil {
			missing = refusal(fmt.Sprintf("Path parameter %s must specify a value.", field[1:len(field)-1]))
		}
		return value
	})
	if missing != nil {
		return nil, missing
	}
	if slices.ContainsFunc(strings.Split(path, "/"), dotSegment.MatchString) {
		return nil, refusal("The request path contains a dot segment.")
	}
	target := strings.TrimSuffix(server, "/") + path
	if len(r.query) != 0 {
		target += "?" + join(r.query, "&")
	}
	var body io.Reader
	if value, ok := values["body"]; ok && o.Body {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
		r.header.Set("Content-Type", "application/json")
	}
	request, err := http.NewRequestWithContext(ctx, o.Method, target, body)
	if err != nil {
		return nil, err
	}
	request.Header = r.header
	if len(r.cookies) != 0 {
		request.Header.Set("Cookie", join(r.cookies, "; "))
	}
	return request, nil
}

func (d Document) serverURL(lookup func(string) (string, bool)) (string, error) {
	server := d.Server
	for _, input := range d.Inputs {
		if input.Secret {
			continue
		}
		value, ok := lookup(input.Name)
		if !ok {
			value = *input.Default
		}
		if input.Choices != nil && !slices.Contains(input.Choices, value) || value == "." || value == ".." {
			return "", refusal(fmt.Sprintf("Environment variable %s is not a permitted server value.", input.Name))
		}
		server = strings.ReplaceAll(server, "{"+input.Name+"}", encode(value))
	}
	if _, err := url.Parse(server); err != nil {
		return "", refusal("The server variables do not form a valid API server URL.")
	}
	return server, nil
}

type argument struct {
	parts  []string
	object bool
}

func argumentOf(value any, escape func(string) string) (argument, error) {
	var result argument
	add := func(part any) error {
		switch part.(type) {
		case nil:
		case []any, map[string]any:
			return refusal("Parameters must be scalars or flat collections.")
		default:
			result.parts = append(result.parts, escape(fmt.Sprint(part)))
		}
		return nil
	}
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			if err := add(item); err != nil {
				return argument{}, err
			}
		}
	case map[string]any:
		result.object = true
		for _, key := range slices.Sorted(maps.Keys(value)) {
			if value[key] == nil {
				continue
			}
			result.parts = append(result.parts, escape(key))
			if err := add(value[key]); err != nil {
				return argument{}, err
			}
		}
	default:
		if err := add(value); err != nil {
			return argument{}, err
		}
	}
	return result, nil
}

func (a argument) simple(explode bool) string {
	if !a.object || !explode {
		return strings.Join(a.parts, ",")
	}
	entries := make([]string, 0, len(a.parts)/2)
	for i := 0; i < len(a.parts); i += 2 {
		entries = append(entries, a.parts[i]+"="+a.parts[i+1])
	}
	return strings.Join(entries, ",")
}

func (a argument) form(name string, explode bool) []pair {
	var pairs []pair
	switch {
	case explode && a.object:
		for i := 0; i < len(a.parts); i += 2 {
			pairs = append(pairs, pair{a.parts[i], a.parts[i+1]})
		}
	case explode:
		for _, part := range a.parts {
			pairs = append(pairs, pair{name, part})
		}
	default:
		pairs = []pair{{name, a.simple(false)}}
	}
	return pairs
}

func join(pairs []pair, delimiter string) string {
	fields := make([]string, len(pairs))
	for i, p := range pairs {
		fields[i] = p.name + "=" + p.value
	}
	return strings.Join(fields, delimiter)
}

func set(pairs []pair, name, value string) []pair {
	return append(slices.DeleteFunc(pairs, func(p pair) bool { return p.name == name }), pair{name, value})
}

func encode(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func secret(name string, lookup func(string) (string, bool)) (string, error) {
	value, _ := lookup(name)
	if value == "" {
		return "", refusal(fmt.Sprintf("Missing credential environment variable %s.", name))
	}
	return value, nil
}

// cookieValue matches a cookie-value: https://www.rfc-editor.org/rfc/rfc6265#section-4.1.1
var cookieValue = regexp.MustCompile(`^[\x21\x23-\x2B\x2D-\x3A\x3C-\x5B\x5D-\x7E]+$`)

func (k APIKey) attach(r *upstream, lookup func(string) (string, bool)) error {
	value, err := secret(k.Input, lookup)
	if err != nil {
		return err
	}
	switch k.In {
	case Header:
		r.header.Set(k.Name, value)
	case Query:
		r.query = set(r.query, encode(k.Name), encode(value))
	case Cookie:
		if !cookieValue.MatchString(value) {
			return refusal(fmt.Sprintf("Environment variable %s is not a valid cookie value.", k.Input))
		}
		r.cookies = set(r.cookies, encode(k.Name), value)
	}
	return nil
}

func (b Bearer) attach(r *upstream, lookup func(string) (string, bool)) error {
	token, err := secret(b.Input, lookup)
	if err != nil {
		return err
	}
	r.header.Set("Authorization", "Bearer "+token)
	return nil
}

func (b Basic) attach(r *upstream, lookup func(string) (string, bool)) error {
	username, err := secret(b.Username, lookup)
	if err != nil {
		return err
	}
	if strings.Contains(username, ":") {
		return refusal(fmt.Sprintf("Environment variable %s must be a username without a colon.", b.Username))
	}
	password, err := secret(b.Password, lookup)
	if err != nil {
		return err
	}
	r.header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	return nil
}
