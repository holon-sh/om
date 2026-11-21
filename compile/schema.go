package compile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// schemaBuilder converts OpenAPI schemas, adding the components they reference to definitions. A
// response builder leaves out the properties that only requests carry.
type schemaBuilder struct {
	doc         *openapi3.T
	definitions map[string]any
	response    bool
}

func (b schemaBuilder) convert(ref *openapi3.SchemaRef) (any, error) {
	if ref == nil {
		return map[string]any{}, nil
	}
	data, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if fields, ok := value.(map[string]any); ok && fields["$schema"] == nil {
		fields["$schema"] = b.doc.JSONSchemaDialect
	}
	return value, b.normalize(value)
}

func (b schemaBuilder) normalize(value any) error {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for _, keyword := range []string{"x-mcp-header", "$recursiveAnchor", "$recursiveRef"} {
		delete(fields, keyword)
	}
	if reference, ok := fields["$ref"].(string); ok {
		reference, err := b.reference(reference)
		if err != nil {
			return err
		}
		if b.doc.IsOpenAPI30() {
			clear(fields)
		}
		fields["$ref"] = reference
	}
	for _, keyword := range []string{"$id", "$anchor", "$dynamicRef", "$dynamicAnchor"} {
		if _, exists := fields[keyword]; exists {
			return fmt.Errorf("schema keyword %s changes reference scope and is unsupported", keyword)
		}
	}
	if dialect, ok := fields["$schema"].(string); ok {
		if !supportedDialect(dialect) {
			return fmt.Errorf("schema dialect %s is unsupported", dialect)
		}
		delete(fields, "$schema")
	}
	if b.doc.IsOpenAPI30() {
		normalize30(fields)
	}
	if b.response {
		b.leaveOutWriteOnly(fields)
	}
	for _, keyword := range []string{"properties", "patternProperties", "dependentSchemas", "$defs", "definitions"} {
		if object, ok := fields[keyword].(map[string]any); ok {
			for _, key := range slices.Sorted(maps.Keys(object)) {
				if err := b.normalize(object[key]); err != nil {
					return err
				}
			}
		}
	}
	for _, keyword := range []string{"prefixItems", "allOf", "anyOf", "oneOf"} {
		if array, ok := fields[keyword].([]any); ok {
			for _, item := range array {
				if err := b.normalize(item); err != nil {
					return err
				}
			}
		}
	}
	for _, keyword := range []string{
		"additionalProperties", "propertyNames", "items", "contains", "not",
		"if", "then", "else", "unevaluatedItems", "unevaluatedProperties", "contentSchema",
	} {
		if err := b.normalize(fields[keyword]); err != nil {
			return err
		}
	}
	return nil
}

func (b schemaBuilder) reference(reference string) (string, error) {
	const components = "#/components/schemas/"
	pointer, err := url.PathUnescape(reference)
	if err != nil || !strings.HasPrefix(pointer, components) {
		return "", errors.New("schema references must target this document's components.schemas")
	}
	fragment := strings.TrimPrefix(pointer, components)
	name, _, _ := strings.Cut(fragment, "/")
	if b.doc.Components == nil || b.doc.Components.Schemas[name] == nil {
		return "", fmt.Errorf("schema reference targets missing component %q", name)
	}
	if _, exists := b.definitions[name]; !exists {
		b.definitions[name] = nil
		value, err := b.convert(b.doc.Components.Schemas[name])
		if err != nil {
			return "", err
		}
		b.definitions[name] = value
	}
	return (&url.URL{Fragment: "/$defs/" + fragment}).String(), nil
}

// leaveOutWriteOnly removes the properties a schema marks [writeOnly], which a response does not carry,
// from its properties and from those it requires.
//
// [writeOnly]: https://json-schema.org/draft/2020-12/json-schema-validation#section-9.4
func (b schemaBuilder) leaveOutWriteOnly(fields map[string]any) {
	properties, _ := fields["properties"].(map[string]any)
	for name, property := range properties {
		if b.writeOnly(property) {
			delete(properties, name)
			if required, ok := fields["required"].([]any); ok {
				fields["required"] = slices.DeleteFunc(required, func(item any) bool { return item == name })
			}
		}
	}
}

func (b schemaBuilder) writeOnly(property any) bool {
	schema, _ := property.(map[string]any)
	reference, _ := schema["$ref"].(string)
	pointer, _ := url.PathUnescape(reference)
	name, referenced := strings.CutPrefix(pointer, "#/components/schemas/")
	return schema["writeOnly"] == true ||
		referenced && b.doc.Components.Schemas[name] != nil && b.doc.Components.Schemas[name].Value.WriteOnly
}

func normalize30(fields map[string]any) {
	if nullable, _ := fields["nullable"].(bool); nullable {
		if kind, ok := fields["type"].(string); ok {
			fields["type"] = []string{kind, "null"}
		}
	}
	delete(fields, "nullable")
	for _, bound := range []struct{ flag, limit string }{
		{"exclusiveMinimum", "minimum"},
		{"exclusiveMaximum", "maximum"},
	} {
		if exclusive, ok := fields[bound.flag].(bool); ok {
			delete(fields, bound.flag)
			if number, ok := fields[bound.limit]; exclusive && ok {
				fields[bound.flag] = number
				delete(fields, bound.limit)
			}
		}
	}
}

func supportedDialect(dialect string) bool {
	switch dialect {
	case "", "https://json-schema.org/draft/2020-12/schema",
		"https://spec.openapis.org/oas/3.1/dialect/base", "https://spec.openapis.org/oas/3.2/dialect/base":
		return true
	}
	return false
}
