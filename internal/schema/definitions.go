package schema

import (
	"maps"
	"net/url"
	"slices"
	"strings"
)

func Subschemas(fields map[string]any) []any {
	var found []any
	for _, keyword := range []string{"properties", "patternProperties", "dependentSchemas", "$defs", "definitions"} {
		if object, ok := fields[keyword].(map[string]any); ok {
			for _, key := range slices.Sorted(maps.Keys(object)) {
				found = append(found, object[key])
			}
		}
	}
	for _, keyword := range []string{"prefixItems", "allOf", "anyOf", "oneOf"} {
		if array, ok := fields[keyword].([]any); ok {
			found = append(found, array...)
		}
	}
	for _, keyword := range []string{
		"additionalProperties", "propertyNames", "items", "contains", "not",
		"if", "then", "else", "unevaluatedItems", "unevaluatedProperties", "contentSchema",
	} {
		found = append(found, fields[keyword])
	}
	return found
}

func Reach(definition func(name string) (any, error), schemas ...any) (map[string]any, error) {
	reached := map[string]any{}
	var visit func(any) error
	visit = func(schema any) error {
		fields, ok := schema.(map[string]any)
		if !ok {
			return nil
		}
		if reference, ok := fields["$ref"].(string); ok {
			pointer, _ := url.PathUnescape(reference)
			name, _, _ := strings.Cut(strings.TrimPrefix(pointer, "#/$defs/"), "/")
			if _, seen := reached[name]; !seen {
				value, err := definition(name)
				if err != nil {
					return err
				}
				reached[name] = value
				if err := visit(value); err != nil {
					return err
				}
			}
		}
		for _, subschema := range Subschemas(fields) {
			if err := visit(subschema); err != nil {
				return err
			}
		}
		return nil
	}
	for _, schema := range schemas {
		if err := visit(schema); err != nil {
			return nil, err
		}
	}
	return reached, nil
}
