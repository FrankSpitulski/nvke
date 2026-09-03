// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package debmanifest

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// scalarFields are the control fields Compare treats as a single opaque
// value rather than a sorted comma list.
var scalarFields = map[string]bool{"Package": true, "Section": true, "Priority": true, "Architecture": true}

// Compare applies dev to a copy of golden, then requires the result to equal
// a copy of actual exactly. It returns a human-readable diff when they do
// not match. It is also an error — even when the manifests match — if any
// non-optional deviation in dev had no effect, since an unused entry is
// exactly the allowlist rot the deviations file exists to prevent. A
// Deviation with Optional set is exempt from that check: it is meant for a
// per-version asymmetry, so matching nothing in a particular comparison is
// expected rather than stale.
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
		// An "optional" deviation exists for a per-version asymmetry (see
		// ParseDeviations): matching nothing in this particular comparison
		// is expected, not allowlist rot.
		if !used[i] && !d.Optional {
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

// pathGlobMatches reports whether p matches pattern using path.Match
// semantics: "*" matches any run of characters within one path segment
// (never crossing "/"), "?" matches one such character, and a literal
// pattern with no metacharacters matches only itself. ParseDeviations
// already validated pattern's syntax, so a malformed pattern here (which
// should not happen) simply matches nothing rather than panicking.
func pathGlobMatches(pattern, p string) bool {
	matched, err := path.Match(pattern, p)
	return err == nil && matched
}

// removeFile deletes every entry in m.Files whose Path matches pattern
// (pathGlobMatches). It returns whether anything matched, which is how
// Compare tells a used deviation from a stale one — a glob is "used" once
// it matches at least one entry, even if it matches several.
func removeFile(m *Manifest, pattern string) bool {
	matched := false
	kept := make([]FileEntry, 0, len(m.Files))
	for _, f := range m.Files {
		if pathGlobMatches(pattern, f.Path) {
			matched = true
			continue
		}
		kept = append(kept, f)
	}
	m.Files = kept
	return matched
}

// addFile copies every entry in actual.Files whose Path matches pattern
// into m.Files, for any not already present there — regardless of that
// entry's type, mode or sha256. It is a used deviation once pattern matches
// at least one entry in actual, whether or not that entry needed copying:
// asserting "this is expected too" for an already-golden path is still the
// glob doing real work when it also covers ones that are not.
func addFile(m *Manifest, pattern string, actual Manifest) bool {
	matched := false
	for _, f := range actual.Files {
		if !pathGlobMatches(pattern, f.Path) {
			continue
		}
		matched = true
		if !containsPath(m.Files, f.Path) {
			m.Files = append(m.Files, f)
		}
	}
	return matched
}

// containsPath reports whether files has an entry at p.
func containsPath(files []FileEntry, p string) bool {
	for _, f := range files {
		if f.Path == p {
			return true
		}
	}
	return false
}

// applyMode overwrites the mode of every entry in m.Files whose Path
// matches pattern, returning whether pattern matched at least one entry.
func applyMode(m *Manifest, pattern, newMode string) bool {
	matched := false
	for i, f := range m.Files {
		if pathGlobMatches(pattern, f.Path) {
			matched = true
			m.Files[i].Mode = newMode
		}
	}
	return matched
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
