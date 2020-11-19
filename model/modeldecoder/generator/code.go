// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package generator

import (
	"bytes"
	"fmt"
	"go/types"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/pkg/errors"
)

const (
	anonymousField = "_"
)

// CodeGenerator creates following struct methods
//   `IsSet() bool`
//   `Reset()`
//   `validate() error`
// on all exported and anonymous structs that are referenced
// by at least one of the root types
type CodeGenerator struct {
	buf      bytes.Buffer
	parsed   *Parsed
	rootObjs []structType

	// keep track of already processed types in case one type is
	// referenced multiple times
	processedTypes map[string]struct{}
}

type validationGenerator func(io.Writer, []structField, structField, bool) error

// NewCodeGenerator takes an importPath and the package name for which
// the type definitions should be loaded.
// The nullableTypePath is used to implement validation rules specific to types
// of the nullable package. The generator creates methods only for types referenced
// directly or indirectly by any of the root types.
func NewCodeGenerator(parsed *Parsed, rootTypes []string) (*CodeGenerator, error) {
	g := CodeGenerator{
		parsed:         parsed,
		rootObjs:       make([]structType, len(rootTypes)),
		processedTypes: make(map[string]struct{}),
	}
	for i := 0; i < len(rootTypes); i++ {
		rootStruct, ok := parsed.structTypes[rootTypes[i]]
		if !ok {
			return nil, fmt.Errorf("object with root key %s not found", rootTypes[i])
		}
		g.rootObjs[i] = rootStruct
	}
	return &g, nil
}

// Generate generates the code for given root structs and all
// dependencies and returns it as bytes.Buffer
func (g *CodeGenerator) Generate() (bytes.Buffer, error) {
	fmt.Fprintf(&g.buf, `
// Code generated by "modeldecoder/generator". DO NOT EDIT.

package %s

import (
	"fmt"
	"encoding/json"
	"github.com/pkg/errors"
	"regexp"
	"unicode/utf8"
)

var (
`[1:], g.parsed.pkgName)
	for _, name := range sortKeys(g.parsed.patternVariables) {
		fmt.Fprintf(&g.buf, `
%sRegexp = regexp.MustCompile(%s)
`[1:], name, name)
	}
	fmt.Fprint(&g.buf, `
)
`[1:])

	// run generator code
	for _, rootObj := range g.rootObjs {
		if err := g.generate(rootObj, ""); err != nil {
			return g.buf, errors.Wrap(err, "code generator")
		}
	}
	return g.buf, nil
}

// create flattened field keys by recursively iterating through the struct types;
// there is only struct local knowledge and no knowledge about the parent,
// deriving the absolute key is not possible in scenarios where one struct
// type is referenced as a field in multiple struct types
func (g *CodeGenerator) generate(st structType, key string) error {
	if _, ok := g.processedTypes[st.name]; ok {
		return nil
	}
	g.processedTypes[st.name] = struct{}{}
	if err := g.generateIsSet(st, key); err != nil {
		return err
	}
	if err := g.generateReset(st, key); err != nil {
		return err
	}
	if err := g.generateValidation(st, key); err != nil {
		return err
	}
	if key != "" {
		key += "."
	}
	for _, field := range st.fields {
		var childTyp types.Type
		switch fieldTyp := field.Type().Underlying().(type) {
		case *types.Map:
			childTyp = fieldTyp.Elem()
		case *types.Slice:
			childTyp = fieldTyp.Elem()
		default:
			childTyp = field.Type()
		}
		if child, ok := g.customStruct(childTyp); ok {
			if err := g.generate(child, fmt.Sprintf("%s%s", key, jsonName(field))); err != nil {
				return err
			}
		}
	}
	return nil
}

// generateIsSet creates `IsSet` methods for struct fields,
// indicating if the fields have been initialized;
// it only considers exported fields, aligned with standard marshal behavior
func (g *CodeGenerator) generateIsSet(structTyp structType, key string) error {
	if len(structTyp.fields) == 0 {
		return fmt.Errorf("unhandled struct %s (does not have any exported fields)", structTyp.name)
	}
	fmt.Fprintf(&g.buf, `
func (val *%s) IsSet() bool {
	return`, structTyp.name)
	if key != "" {
		key += "."
	}
	prefix := ``
	for i := 0; i < len(structTyp.fields); i++ {
		f := structTyp.fields[i]
		if !f.Exported() {
			continue
		}
		switch t := f.Type().Underlying().(type) {
		case *types.Slice, *types.Map:
			fmt.Fprintf(&g.buf, `%s len(val.%s) > 0`, prefix, f.Name())
		case *types.Struct:
			fmt.Fprintf(&g.buf, `%s val.%s.IsSet()`, prefix, f.Name())
		default:
			return fmt.Errorf("unhandled type %T for IsSet() for '%s%s'", t, key, jsonName(f))
		}
		prefix = ` ||`
	}
	fmt.Fprint(&g.buf, `
}
`)
	return nil
}

// generateReset creates `Reset` methods for struct fields setting them to
// their zero values or calling their `Reset` methods
// it only considers exported fields
func (g *CodeGenerator) generateReset(structTyp structType, key string) error {
	fmt.Fprintf(&g.buf, `
func (val *%s) Reset() {
`, structTyp.name)
	if key != "" {
		key += "."
	}
	for _, f := range structTyp.fields {
		if !f.Exported() {
			continue
		}
		switch t := f.Type().Underlying().(type) {
		case *types.Slice:
			// the slice len is set to zero, not returning the underlying
			// memory to the garbage collector; when the size of slices differs
			// this potentially leads to keeping more memory allocated than required;

			// if slice type is a model struct,
			// call its Reset() function
			if _, ok := g.customStruct(t.Elem()); ok {
				fmt.Fprintf(&g.buf, `
for i := range val.%s{
	val.%s[i].Reset()
}
`[1:], f.Name(), f.Name())
			}
			// then reset size of slice to 0
			fmt.Fprintf(&g.buf, `
val.%s = val.%s[:0]
`[1:], f.Name(), f.Name())

		case *types.Map:
			// the map is cleared, not returning the underlying memory to
			// the garbage collector; when map size differs this potentially
			// leads to keeping more memory allocated than required
			fmt.Fprintf(&g.buf, `
for k := range val.%s {
	delete(val.%s, k)
}
`[1:], f.Name(), f.Name())

		case *types.Struct:
			fmt.Fprintf(&g.buf, `
val.%s.Reset()
`[1:], f.Name())
		default:
			return fmt.Errorf("unhandled type %T for Reset() for '%s%s'", t, key, jsonName(f))
		}
	}
	fmt.Fprint(&g.buf, `
}
`[1:])
	return nil
}

// generateValidation creates `validate` methods for struct fields
// it only considers exported and anonymous fields
func (g *CodeGenerator) generateValidation(structTyp structType, key string) error {
	fmt.Fprintf(&g.buf, `
func (val *%s) validate() error {
`, structTyp.name)
	var isRoot bool
	for _, rootObjs := range g.rootObjs {
		if structTyp.name == rootObjs.name {
			isRoot = true
			break
		}
	}
	if !isRoot {
		fmt.Fprint(&g.buf, `
if !val.IsSet() {
	return nil
}
`[1:])
	}

	var validation validationGenerator
	for i := 0; i < len(structTyp.fields); i++ {
		f := structTyp.fields[i]
		// according to https://golang.org/pkg/go/types/#Var.Anonymous
		// f.Anonymous() actually checks if f is embedded, not anonymous,
		// so we need to do a name check instead
		if !f.Exported() && f.Name() != anonymousField {
			continue
		}
		var custom bool
		switch f.Type().String() {
		case nullableTypeString:
			validation = generateNullableStringValidation
		case nullableTypeInt:
			validation = generateNullableIntValidation
		case nullableTypeFloat64:
			// right now we can reuse the validation rules for int
			// and only introduce dedicated rules for float64 when they diverge
			validation = generateNullableIntValidation
		case nullableTypeInterface:
			validation = generateNullableInterfaceValidation
		default:
			switch t := f.Type().Underlying().(type) {
			case *types.Slice:
				validation = generateSliceValidation
				_, custom = g.customStruct(t.Elem())
			case *types.Map:
				validation = generateMapValidation
				_, custom = g.customStruct(t.Elem())
			case *types.Struct:
				validation = generateStructValidation
				_, custom = g.customStruct(f.Type())
			default:
				return errors.Wrap(fmt.Errorf("unhandled type %T", t), flattenName(key, f))
			}
		}
		if err := validation(&g.buf, structTyp.fields, f, custom); err != nil {
			return errors.Wrap(err, flattenName(key, f))
		}
	}
	fmt.Fprint(&g.buf, `
return nil
}
`[1:])
	return nil
}

func (g *CodeGenerator) customStruct(typ types.Type) (t structType, ok bool) {
	t, ok = g.parsed.structTypes[typ.String()]
	return
}

func flattenName(key string, f structField) string {
	if key != "" {
		key += "."
	}
	return fmt.Sprintf("%s%s", key, jsonName(f))
}

func jsonName(f structField) string {
	parts := parseTag(f.tag, "json")
	if len(parts) == 0 {
		return strings.ToLower(f.Name())
	}
	return parts[0]
}

func parseTag(structTag reflect.StructTag, tagName string) []string {
	tag, ok := structTag.Lookup(tagName)
	if !ok {
		return []string{}
	}
	if tag == "-" {
		return nil
	}
	return strings.Split(tag, ",")
}

func sortKeys(input map[string]string) []string {
	keys := make(sort.StringSlice, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	keys.Sort()
	return keys
}