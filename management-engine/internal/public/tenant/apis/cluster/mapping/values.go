// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mapping converts only reviewed public fields to and from storage.
package mapping

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// FieldError reports a public field and a safe reason without echoing input values.
type FieldError struct {
	Field       string
	Description string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Description }

func required(path string) error {
	return &FieldError{Field: path, Description: "is required"}
}

func validateString(value, path string, minLength, maxLength int, pattern *regexp.Regexp) error {
	length := utf8.RuneCountInString(value)
	if length < minLength || (maxLength >= 0 && length > maxLength) {
		return &FieldError{Field: path, Description: "has invalid length"}
	}
	if pattern != nil && !pattern.MatchString(value) {
		return &FieldError{Field: path, Description: "has invalid format"}
	}
	return nil
}

func readString(src map[string]any, path ...string) (*string, error) {
	value, found, err := unstructured.NestedString(src, path...)
	if err != nil {
		return nil, fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
	}
	if !found {
		return nil, nil
	}
	return &value, nil
}

func readStrings(src map[string]any, path ...string) ([]string, error) {
	value, _, err := unstructured.NestedStringSlice(src, path...)
	if err != nil {
		return nil, fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
	}
	return value, nil
}

func hasObject(src map[string]any, path ...string) (bool, error) {
	value, found, err := unstructured.NestedFieldNoCopy(src, path...)
	if err != nil {
		return false, fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
	}
	if found {
		if _, ok := value.(map[string]any); !ok {
			return false, fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
		}
	}
	return found, nil
}

func putValue(dst map[string]any, value any, path ...string) error {
	if err := unstructured.SetNestedField(dst, value, path...); err != nil {
		return fmt.Errorf("invalid stored field %s", strings.Join(path, "."))
	}
	return nil
}

func replaceValue(dst, values map[string]any, path ...string) error {
	value, found, err := unstructured.NestedFieldNoCopy(values, path...)
	if err != nil {
		return fmt.Errorf("read replacement field %s: %w", strings.Join(path, "."), err)
	}
	if !found {
		unstructured.RemoveNestedField(dst, path...)
		return nil
	}
	return putValue(dst, value, path...)
}
