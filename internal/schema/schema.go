package schema

import (
	"bytes"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Compiler map[string]*jsonschema.Schema

func (c Compiler) Compile(schema []byte) (*jsonschema.Schema, error) {
	if compiled, ok := c[string(schema)]; ok {
		return compiled, nil
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const location = "urn:om:schema"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, err
	}
	c[string(schema)] = compiled
	return compiled, nil
}
