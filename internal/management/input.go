package management

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Input definitions are the request fields and constraints from the release OpenAPI.
//
//go:embed inputs.json
var definitions []byte

type field struct {
	Type                                                 string
	Default                                              any
	Enum                                                 []any
	AnyOf                                                []field
	Items                                                *field
	Minimum, Maximum, ExclusiveMinimum, ExclusiveMaximum *float64
	MinLength, MaxLength                                 *int
}
type model struct {
	Required   []string
	Properties map[string]field
}

var models = func() map[string]model {
	var v map[string]model
	if e := json.Unmarshal(definitions, &v); e != nil {
		panic(e)
	}
	return v
}()

func accepts(f field, v any) bool {
	if len(f.AnyOf) > 0 {
		for _, alt := range f.AnyOf {
			if accepts(alt, v) {
				return true
			}
		}
		return false
	}
	if len(f.Enum) > 0 {
		for _, a := range f.Enum {
			if fmt.Sprint(a) == fmt.Sprint(v) {
				return true
			}
		}
		return false
	}
	switch f.Type {
	case "null":
		return v == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "string":
		s, ok := v.(string)
		n := utf8.RuneCountInString(s)
		return ok && (f.MinLength == nil || n >= *f.MinLength) && (f.MaxLength == nil || n <= *f.MaxLength)
	case "array":
		a, ok := v.([]any)
		if !ok {
			return false
		}
		if f.Items != nil {
			for _, x := range a {
				if !accepts(*f.Items, x) {
					return false
				}
			}
		}
		return true
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "integer", "number":
		n, ok := v.(float64)
		return ok && (f.Type != "integer" || math.Trunc(n) == n) && (f.Minimum == nil || n >= *f.Minimum) && (f.Maximum == nil || n <= *f.Maximum) && (f.ExclusiveMinimum == nil || n > *f.ExclusiveMinimum) && (f.ExclusiveMaximum == nil || n < *f.ExclusiveMaximum)
	}
	return true
}
func input(name string, data row) (row, error) {
	def, exists := models[name]
	if !exists {
		panic("Unknown input model " + name)
	}
	normalized := row{}
	for _, key := range def.Required {
		if data[key] == nil {
			return nil, invalid(key + " is required")
		}
	}
	for key, schema := range def.Properties {
		v, ok := data[key]
		if !ok {
			if schema.Default != nil {
				normalized[key] = schema.Default
			}
			continue
		}
		if !accepts(schema, v) {
			return nil, invalid("Invalid " + key)
		}
		if key == "name" || key == "material" {
			if val, ok := v.(string); ok {
				v = strings.TrimSpace(val)
				if v == "" {
					return nil, invalid(key + " cannot be blank")
				}
			}
		}
		normalized[key] = v
	}
	return normalized, nil
}
func (r request) input(name string) (row, error) {
	d, e := r.body()
	if e != nil {
		return nil, e
	}
	return input(name, d)
}
