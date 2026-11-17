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
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type refusal string

func (r refusal) Error() string { return string(r) }

func failure(err error) *mcp.CallToolResult {
	var refused refusal
	if !errors.As(err, &refused) {
		refused = "Could not complete the upstream request."
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(refused)}}, IsError: true}
}

func (s served) call(ctx context.Context, o Operation, arguments json.RawMessage) *mcp.CallToolResult {
	if arguments == nil {
		arguments = json.RawMessage("{}")
	}
	schema, err := compileSchema(o.InputSchema)
	if err != nil {
		return failure(refusal(fmt.Sprintf("The input schema of tool %s is not valid JSON Schema 2020-12: %v", o.Name, err)))
	}
	values, err := jsonschema.UnmarshalJSON(bytes.NewReader(arguments))
	if err == nil {
		err = schema.Validate(values)
	}
	if err != nil {
		return failure(refusal(fmt.Sprintf("Input validation error: Invalid arguments for tool %s: %v", o.Name, err)))
	}
	request, err := s.request(ctx, o, values.(map[string]any), arguments)
	if err != nil {
		return failure(err)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return failure(err)
	}
	defer response.Body.Close()
	text, err := responseText(response)
	switch {
	case err != nil:
		return failure(err)
	case response.StatusCode/100 != 2 && text != "":
		return failure(refusal(fmt.Sprintf("HTTP %d: %s", response.StatusCode, text)))
	case response.StatusCode/100 != 2:
		return failure(refusal(fmt.Sprintf("HTTP %d", response.StatusCode)))
	case text == "":
		return &mcp.CallToolResult{Content: []mcp.Content{}}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func compileSchema(schema json.RawMessage) (*jsonschema.Schema, error) {
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
	return compiler.Compile(location)
}

type unenforced string

func (p unenforced) String() string        { return string(p) }
func (unenforced) MatchString(string) bool { return true }

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
	header  http.Header
	query   []pair
	cookies []pair
}

type pair struct{ name, value string }

var placeholder = regexp.MustCompile(`\{([^{}]+)\}`)
var dotSegment = regexp.MustCompile(`(?i)^(?:\.|%2e){1,2}$`)

func (s served) request(ctx context.Context, o Operation, values map[string]any, arguments json.RawMessage) (*http.Request, error) {
	server, err := s.serverURL()
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	r := upstream{header: http.Header{}}
	for _, parameter := range o.Parameters {
		escape := encode
		if parameter.In == Header {
			escape = func(value string) string { return value }
		}
		value, err := argumentOf(values[parameter.Name], escape)
		if err != nil {
			return nil, err
		}
		if value.items == nil && value.entries == nil {
			continue
		}
		name := encode(parameter.Name)
		switch parameter.In {
		case Path:
			paths[parameter.Name] = value.simple(parameter.Explode)
		case Header:
			r.header.Set(parameter.Name, value.simple(parameter.Explode))
		case Query:
			r.query = append(r.query, value.form(name, parameter.Explode)...)
		case Cookie:
			r.cookies = append(r.cookies, value.form(name, parameter.Explode)...)
		case DeepObject:
			if value.entries == nil {
				return nil, refusal("A deepObject parameter must be an object.")
			}
			for _, entry := range value.entries {
				r.query = append(r.query, pair{name + "%5B" + entry.name + "%5D", entry.value})
			}
		}
	}
	for _, credential := range o.Security {
		if err := credential.attach(&r, s.lookup); err != nil {
			return nil, err
		}
	}
	var missing error
	path := placeholder.ReplaceAllStringFunc(o.Path, func(field string) string {
		name := strings.Trim(field, "{}")
		value, ok := paths[name]
		if !ok && missing == nil {
			missing = refusal(fmt.Sprintf("Path parameter %s must specify a value.", name))
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
	if _, ok := values["body"]; ok && o.Body {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(arguments, &fields); err != nil {
			return nil, err
		}
		body = bytes.NewReader(fields["body"])
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

func (s served) serverURL() (string, error) {
	server := s.Server
	for _, input := range s.Inputs {
		if input.Default == nil {
			continue
		}
		value, ok := s.lookup(input.Name)
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
	items   []string
	entries []pair
}

func argumentOf(value any, escape func(string) string) (argument, error) {
	var result argument
	scalar := func(part any) (string, error) {
		switch part.(type) {
		case []any, map[string]any:
			return "", refusal("Parameters must be scalars or flat collections.")
		}
		return escape(fmt.Sprint(part)), nil
	}
	if object, ok := value.(map[string]any); ok {
		for _, key := range slices.Sorted(maps.Keys(object)) {
			if object[key] == nil {
				continue
			}
			part, err := scalar(object[key])
			if err != nil {
				return argument{}, err
			}
			result.entries = append(result.entries, pair{escape(key), part})
		}
		return result, nil
	}
	items, ok := value.([]any)
	if !ok {
		items = []any{value}
	}
	for _, item := range items {
		if item == nil {
			continue
		}
		part, err := scalar(item)
		if err != nil {
			return argument{}, err
		}
		result.items = append(result.items, part)
	}
	return result, nil
}

func (a argument) simple(explode bool) string {
	if explode && a.entries != nil {
		return join(a.entries, ",")
	}
	parts := a.items
	for _, entry := range a.entries {
		parts = append(parts, entry.name, entry.value)
	}
	return strings.Join(parts, ",")
}

func (a argument) form(name string, explode bool) []pair {
	if !explode {
		return []pair{{name, a.simple(false)}}
	}
	pairs := a.entries
	for _, item := range a.items {
		pairs = append(pairs, pair{name, item})
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
