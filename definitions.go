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
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func (d Document) standalone(o Operation) (Operation, error) {
	var err error
	if o.InputSchema, err = d.expand(o.InputSchema, false); err != nil {
		return Operation{}, err
	}
	if o.OutputSchema != nil {
		if o.OutputSchema, err = d.expand(o.OutputSchema, true); err != nil {
			return Operation{}, err
		}
	}
	return o, nil
}

func (d Document) expand(root json.RawMessage, result bool) (json.RawMessage, error) {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(root))
	if err != nil {
		return nil, err
	}
	reached, err := schema.Reach(func(name string) (any, error) {
		definition, err := jsonschema.UnmarshalJSON(bytes.NewReader(d.Definitions[name]))
		if err != nil {
			return nil, fmt.Errorf("definition %q: %w", name, err)
		}
		return definition, nil
	}, value)
	if err != nil {
		return nil, err
	}
	if fields, ok := value.(map[string]any); ok && len(reached) != 0 {
		fields["$defs"] = reached
	}
	if result {
		definitions(reached).leaveOutWriteOnly(value)
	}
	return json.Marshal(value)
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
	for _, subschema := range schema.Subschemas(fields, "not", "if", "propertyNames") {
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
		if d.applies(d.target(reference), test, seen) {
			return true
		}
	}
	branches, _ := fields["allOf"].([]any)
	return slices.ContainsFunc(branches, func(branch any) bool { return d.applies(branch, test, seen) })
}

func (d definitions) target(reference string) any {
	pointer, _ := url.PathUnescape(strings.TrimPrefix(reference, "#/$defs"))
	parsed, _ := jsonpointer.New(pointer)
	target, _, _ := parsed.Get(d)
	return target
}
