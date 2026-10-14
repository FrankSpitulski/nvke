// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

type policy struct {
	Version  string         `json:"version"`
	Fields   []field        `json:"fields"`
	Defaults []defaultValue `json:"defaults"`
}

type defaultValue struct {
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

type field struct {
	Path   string   `json:"path"`
	Tag    int      `json:"tag"`
	Access []string `json:"access"`
}

type record struct {
	field
	Name     string                          `json:"name"`
	Required bool                            `json:"required"`
	Schema   apiextensionsv1.JSONSchemaProps `json:"schema"`
	Default  json.RawMessage                 `json:"default,omitempty"`
}

type node struct {
	record
	children    []*node
	description string
}

type model struct {
	root              *node
	nodes             []*node
	resourceAdmission apiextensionsv1.JSONSchemaProps
	defaults          []defaultValue
	createAdmission   map[string]record
}

const (
	schemaObject = "object"
	schemaString = "string"
	schemaArray  = "array"
	modeRead     = "read"
	modeCreate   = "create"
	modeReplace  = "replace"
)

var pathPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(\.[a-z][a-zA-Z0-9]*)*$`)

func compile(crdData, policyData []byte) (*model, error) {
	var exposure policy
	decoder := json.NewDecoder(bytes.NewReader(policyData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&exposure); err != nil {
		return nil, fmt.Errorf("decode exposure policy: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("exposure policy must contain exactly one JSON value")
	}
	if exposure.Version != "v1" || len(exposure.Fields) == 0 {
		return nil, fmt.Errorf("expected a nonempty v1 exposure policy")
	}
	spec, resource, err := storedSchemas(crdData)
	if err != nil {
		return nil, err
	}
	root := &node{Schema: constraintsOnly(spec)}
	result := &model{root: root, resourceAdmission: constraintsOnly(resource)}
	indexed := map[string]*node{"": root}
	slices.SortFunc(exposure.Fields, func(a, b field) int { return strings.Compare(a.Path, b.Path) })
	for _, selected := range exposure.Fields {
		entry, err := compileField(selected, spec, indexed)
		if err != nil {
			return nil, err
		}
		indexed[entry.Path] = entry
		result.nodes = append(result.nodes, entry)
	}
	for _, entry := range indexed {
		slices.SortFunc(entry.children, func(a, b *node) int { return a.Tag - b.Tag })
		if entry.Schema.Type != schemaObject || entry == root {
			continue
		}
		if len(entry.children) == 0 {
			return nil, fmt.Errorf("exposed object %s has no reviewed child fields", entry.Path)
		}
		// Every writable object must reach a populated leaf through required children.
		// Child objects are checked independently; private defaults do not establish public presence.
		for _, access := range entry.Access {
			if access != modeRead && !slices.ContainsFunc(entry.children, func(child *node) bool {
				return child.Required && slices.Contains(child.Access, access)
			}) {
				return nil, fmt.Errorf("writable object %s requires a required %s child to preserve presence", entry.Path, access)
			}
		}
	}
	if err := compileDefaults(result, exposure.Defaults, spec, indexed); err != nil {
		return nil, err
	}
	return result, nil
}

func storedSchemas(data []byte) (*apiextensionsv1.JSONSchemaProps, *apiextensionsv1.JSONSchemaProps, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &crd); err != nil {
		return nil, nil, fmt.Errorf("decode internal CRD: %w", err)
	}
	if crd.Spec.Group != "internal.nvke.nvidia.com" || crd.Spec.Names.Kind != "NVKECluster" ||
		crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		return nil, nil, fmt.Errorf("expected namespaced internal.nvke.nvidia.com NVKECluster CRD")
	}
	for _, version := range crd.Spec.Versions {
		if version.Name == "v1" && version.Served && version.Schema != nil && version.Schema.OpenAPIV3Schema != nil {
			spec, found := version.Schema.OpenAPIV3Schema.Properties["spec"]
			if found && spec.Type == schemaObject {
				return &spec, version.Schema.OpenAPIV3Schema, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("internal v1 spec schema unavailable")
}

func compileField(selected field, spec *apiextensionsv1.JSONSchemaProps, indexed map[string]*node) (*node, error) {
	if !pathPattern.MatchString(selected.Path) || selected.Tag < 1 || selected.Tag > 536870911 ||
		(selected.Tag >= 19000 && selected.Tag <= 19999) {
		return nil, fmt.Errorf("invalid field path or protobuf tag: %s", selected.Path)
	}
	if _, exists := indexed[selected.Path]; exists {
		return nil, fmt.Errorf("duplicate exposure path: %s", selected.Path)
	}
	parts := strings.Split(selected.Path, ".")
	parent := indexed[strings.Join(parts[:len(parts)-1], ".")]
	if parent == nil {
		return nil, fmt.Errorf("exposed field %s requires an explicit parent policy", selected.Path)
	}
	schema, required, err := resolveSchema(spec, parts)
	if err != nil {
		return nil, err
	}
	if err := supportedSchema(schema); err != nil {
		return nil, fmt.Errorf("unsupported exposed field %s: %w", selected.Path, err)
	}
	if err := validateAccess(selected, parent); err != nil {
		return nil, err
	}
	// Repeated protobuf fields cannot preserve absent versus empty input lists.
	if schema.Type == schemaArray && !required &&
		(slices.Contains(selected.Access, modeCreate) || slices.Contains(selected.Access, modeReplace)) {
		return nil, fmt.Errorf("optional writable array %s requires explicit presence semantics", selected.Path)
	}
	name := snake(parts[len(parts)-1])
	for _, sibling := range parent.children {
		if sibling.Tag == selected.Tag || sibling.Name == name {
			return nil, fmt.Errorf("duplicate sibling tag or public name: %s", selected.Path)
		}
	}
	entry := &node{
		field: selected, Name: name, Required: required, Schema: constraintsOnly(schema),
		description: schema.Description,
	}
	parent.children = append(parent.children, entry)
	return entry, nil
}

func constraintsOnly(schema *apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	constraints := *schema.DeepCopy()
	constraints.Description = ""
	if constraints.Items != nil && constraints.Items.Schema != nil {
		constraints.Items.Schema.Description = ""
	}
	constraints.Properties = nil
	// A newly required private sibling can invalidate public creates even though it stays masked.
	slices.Sort(constraints.Required)
	return constraints
}

func resolveSchema(
	spec *apiextensionsv1.JSONSchemaProps,
	parts []string,
) (*apiextensionsv1.JSONSchemaProps, bool, error) {
	current := spec
	required := false
	for _, part := range parts {
		required = slices.Contains(current.Required, part)
		child, exists := current.Properties[part]
		if !exists {
			return nil, false, fmt.Errorf("exposure path missing from internal schema: %s", strings.Join(parts, "."))
		}
		current = &child
	}
	return current, required, nil
}

func validateAccess(selected field, parent *node) error {
	if len(selected.Access) == 0 {
		return fmt.Errorf("missing field access: %s", selected.Path)
	}
	seen := map[string]bool{}
	for _, access := range selected.Access {
		if (access != modeCreate && access != modeRead && access != modeReplace) || seen[access] {
			return fmt.Errorf("unsupported or duplicate access on %s", selected.Path)
		}
		if parent.Path != "" && !slices.Contains(parent.Access, access) {
			return fmt.Errorf("parent does not expose %s for %s", access, selected.Path)
		}
		seen[access] = true
	}
	slices.Sort(selected.Access)
	return nil
}

func supportedSchema(schema *apiextensionsv1.JSONSchemaProps) error {
	if schema.Nullable || schema.XIntOrString || schema.XPreserveUnknownFields != nil ||
		schema.AdditionalProperties != nil || len(schema.AllOf)+len(schema.AnyOf)+len(schema.OneOf) > 0 ||
		schema.Not != nil || schema.Ref != nil || schema.Default != nil || len(schema.Enum) != 0 || schema.Format != "" {
		return fmt.Errorf("unreviewed nullable, union, map, reference, default, enum or format")
	}
	switch schema.Type {
	case schemaObject:
		return nil
	case schemaString:
		if _, err := regexp.Compile(schema.Pattern); err != nil {
			return fmt.Errorf("invalid string pattern: %w", err)
		}
		return nil
	case schemaArray:
		if schema.XListType == nil || *schema.XListType != "atomic" || schema.Items == nil ||
			schema.Items.Schema == nil || schema.Items.Schema.Type != schemaString {
			return fmt.Errorf("only atomic string collections have a defined public contract")
		}
		return supportedSchema(schema.Items.Schema)
	default:
		return fmt.Errorf("unsupported scalar type %q", schema.Type)
	}
}

func snake(value string) string {
	var out strings.Builder
	for i, r := range value {
		if r >= 'A' && r <= 'Z' {
			// Keep acronym runs together, but separate their final capital from a following word.
			if i > 0 && (value[i-1] >= 'a' && value[i-1] <= 'z' ||
				value[i-1] >= '0' && value[i-1] <= '9' ||
				i+1 < len(value) && value[i+1] >= 'a' && value[i+1] <= 'z') {
				out.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		out.WriteRune(r)
	}
	return out.String()
}

func title(value string) string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '.' || r == '_' })
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "")
}
