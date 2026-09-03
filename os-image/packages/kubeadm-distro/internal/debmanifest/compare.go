// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package debmanifest

import (
	"fmt"
	"sort"
	"strings"
)

// scalarFields are the control fields Compare treats as a single opaque
// value rather than a sorted comma list.
var scalarFields = map[string]bool{"Package": true, "Section": true, "Priority": true, "Architecture": true}

// Compare applies dev to a copy of golden, then requires the result to equal
// a copy of actual exactly. It returns a human-readable diff when they do
// not match. It is also an error — even when the manifests match — if any
// deviation in dev had no effect, since an unused entry is exactly the
// allowlist rot the deviations file exists to prevent.
//
// Compare never mutates golden, actual, or the Deviation values in dev: it
// works on cloned manifests throughout, and tracks which deviations fired in
// a local slice rather than on the shared *Deviation structs, since the same
// []*Deviation is reused across every .deb a caller like "debmanifest check"
// compares in one run.
func Compare(golden, actual Manifest, dev []*Deviation) (string, error) {
	working := cloneManifest(golden)
	actualClone := cloneManifest(actual)

	used := make([]bool, len(dev))
	for i, d := range dev {
		switch d.Kind {
		case "removed":
			used[i] = removeFile(&working, d.Values[0])
		case "added":
			used[i] = addFile(&working, d.Values[0], actualClone)
		case "mode":
			used[i] = applyMode(&working, d.Values[0], d.Values[1])
		case "field":
			used[i] = applyField(&working, d.Field, d.Op, d.Values)
		case "scripts":
			working.Scripts, used[i] = applyListOp(working.Scripts, d.Op, d.Values)
		case "conffiles":
			working.Conffiles, used[i] = applyListOp(working.Conffiles, d.Op, d.Values)
		default:
			return "", fmt.Errorf("internal error: unknown deviation kind %q", d.Kind)
		}
	}
	working.normalize()
	actualClone.normalize()

	var unused []string
	for i, d := range dev {
		if !used[i] {
			unused = append(unused, fmt.Sprintf("line %d: %q — %s", d.line, d.raw, d.Reason))
		}
	}

	diff := diffManifests(working, actualClone)
	if diff != "" {
		if len(unused) > 0 {
			return diff, fmt.Errorf("manifest mismatch, and %d deviation(s) had no effect:\n%s", len(unused), strings.Join(unused, "\n"))
		}
		return diff, fmt.Errorf("manifest does not match golden after applying deviations")
	}
	if len(unused) > 0 {
		return "", fmt.Errorf("%d deviation(s) in deviations.txt had no effect (allowlist rot):\n%s", len(unused), strings.Join(unused, "\n"))
	}
	return "", nil
}

func cloneManifest(m Manifest) Manifest {
	out := Manifest{Control: make(map[string][]string, len(m.Control))}
	for k, v := range m.Control {
		out.Control[k] = append([]string(nil), v...)
	}
	out.Conffiles = append([]string(nil), m.Conffiles...)
	out.Scripts = append([]string(nil), m.Scripts...)
	out.Files = append([]FileEntry(nil), m.Files...)
	return out
}

// removeFile deletes the file at path from m.Files. It returns whether the
// path was actually present, which is how Compare tells a used deviation
// from a stale one.
func removeFile(m *Manifest, path string) bool {
	for i, f := range m.Files {
		if f.Path == path {
			m.Files = append(m.Files[:i], m.Files[i+1:]...)
			return true
		}
	}
	return false
}

// addFile copies path's entry from actual into m.Files. It is a used
// deviation only if the path is missing from m.Files and present in actual —
// otherwise there was nothing for it to paper over.
func addFile(m *Manifest, path string, actual Manifest) bool {
	for _, f := range m.Files {
		if f.Path == path {
			return false
		}
	}
	for _, f := range actual.Files {
		if f.Path == path {
			m.Files = append(m.Files, f)
			return true
		}
	}
	return false
}

// applyMode overwrites the mode of the file entry at path, returning whether
// it changed anything.
func applyMode(m *Manifest, path, newMode string) bool {
	for i, f := range m.Files {
		if f.Path == path {
			if f.Mode == newMode {
				return false
			}
			m.Files[i].Mode = newMode
			return true
		}
	}
	return false
}

// applyField applies a field deviation to m.Control[field], returning
// whether it changed anything. op is guaranteed "=" when field is scalar:
// ParseDeviations rejects "+="/"-=" on a scalar field at parse time.
func applyField(m *Manifest, field, op string, values []string) bool {
	before := append([]string(nil), m.Control[field]...)
	if scalarFields[field] {
		m.Control[field] = values
	} else {
		m.Control[field], _ = applyListOp(m.Control[field], op, values)
	}
	return !stringSliceEqual(before, m.Control[field])
}

func applyListOp(current []string, op string, values []string) ([]string, bool) {
	switch op {
	case "=":
		return append([]string(nil), values...), !stringSliceEqualUnordered(current, values)
	case "+=":
		out := append([]string(nil), current...)
		changed := false
		for _, v := range values {
			if !contains(out, v) {
				out = append(out, v)
				changed = true
			}
		}
		return out, changed
	case "-=":
		out := make([]string, 0, len(current))
		changed := false
		for _, c := range current {
			if contains(values, c) {
				changed = true
				continue
			}
			out = append(out, c)
		}
		return out, changed
	}
	return current, false
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSliceEqualUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	return stringSliceEqual(as, bs)
}

// diffManifests renders golden and actual as their canonical text and
// returns a line-level diff ("-" golden-only lines, "+" actual-only lines).
// It returns "" when the manifests are identical.
func diffManifests(golden, actual Manifest) string {
	g := strings.Split(strings.TrimSuffix(golden.String(), "\n"), "\n")
	a := strings.Split(strings.TrimSuffix(actual.String(), "\n"), "\n")
	if stringSliceEqual(g, a) {
		return ""
	}
	return diffLines(g, a)
}

// diffLines is a small LCS-based line diff: "-" for lines only in golden,
// "+" for lines only in actual, no context lines.
func diffLines(golden, actual []string) string {
	m, n := len(golden), len(actual)
	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if golden[i] == actual[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var b strings.Builder
	i, j := 0, 0
	for i < m && j < n {
		switch {
		case golden[i] == actual[j]:
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			fmt.Fprintf(&b, "-%s\n", golden[i])
			i++
		default:
			fmt.Fprintf(&b, "+%s\n", actual[j])
			j++
		}
	}
	for ; i < m; i++ {
		fmt.Fprintf(&b, "-%s\n", golden[i])
	}
	for ; j < n; j++ {
		fmt.Fprintf(&b, "+%s\n", actual[j])
	}
	return b.String()
}
