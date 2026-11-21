package om

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/go-openapi/jsonpointer"
	"github.com/holon-sh/om/internal/schema"
)

func (d Document) standalone(root json.RawMessage, result bool) (json.RawMessage, error) {
	value, err := decode(root)
	if err != nil {
		return nil, err
	}
	reached, err := schema.Reach(func(name string) (any, error) {
		definition, err := decode(d.Definitions[name])
		if err != nil {
			return nil, fmt.Errorf("definition %q: %w", name, err)
		}
		return definition, nil
	}, value)
	if err != nil {
		return nil, err
	}
	if len(reached) != 0 {
		value.(map[string]any)["$defs"] = reached
	}
	if result {
		definitions(reached).leaveOutWriteOnly(value)
	}
	return json.Marshal(value)
}

func decode(data json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}

type definitions map[string]any

func (d definitions) leaveOutWriteOnly(value any) {
	fields, ok := value.(map[string]any)
	if !ok {
		return
	}
	if required, ok := fields["required"].([]any); ok {
		fields["required"] = slices.DeleteFunc(required, func(name any) bool {
			return d.applies(fields, func(object map[string]any) bool {
				properties, _ := object["properties"].(map[string]any)
				return d.applies(properties[name.(string)], func(property map[string]any) bool {
					return property["writeOnly"] == true
				}, map[string]bool{})
			}, map[string]bool{})
		})
	}
	for _, subschema := range schema.Subschemas(fields) {
		d.leaveOutWriteOnly(subschema)
	}
}

func (d definitions) applies(value any, test func(map[string]any) bool, seen map[string]bool) bool {
	fields, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if test(fields) {
		return true
	}
	if reference, ok := fields["$ref"].(string); ok && !seen[reference] {
		seen[reference] = true
		pointer, _ := url.PathUnescape(strings.TrimPrefix(reference, "#/$defs"))
		parsed, _ := jsonpointer.New(pointer)
		target, _, _ := parsed.Get(map[string]any(d))
		if d.applies(target, test, seen) {
			return true
		}
	}
	branches, _ := fields["allOf"].([]any)
	return slices.ContainsFunc(branches, func(branch any) bool { return d.applies(branch, test, seen) })
}

func rebase(value any, base string) {
	fields, ok := value.(map[string]any)
	if !ok {
		return
	}
	if reference, ok := fields["$ref"].(string); ok && strings.HasPrefix(reference, "#/$defs/") {
		fields["$ref"] = base + reference
	}
	for _, subschema := range schema.Subschemas(fields) {
		rebase(subschema, base)
	}
}
