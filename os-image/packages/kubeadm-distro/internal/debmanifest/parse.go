// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package debmanifest

import (
	"fmt"
	"strings"
)

// ParseManifest reads the debmanifest.sh text format — the same shape
// Manifest.String prints — back into a normalized Manifest. It is used to
// load the committed testdata/golden/*.manifest files.
func ParseManifest(text string) (Manifest, error) {
	m := Manifest{Control: map[string][]string{}}

	section := ""
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\n")
		switch line {
		case "## control", "## conffiles", "## scripts", "## files":
			section = line
			continue
		}
		if line == "" && section == "" {
			continue
		}

		switch section {
		case "## control":
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			field, value, ok := strings.Cut(line, ": ")
			if !ok {
				return Manifest{}, fmt.Errorf("manifest line %d: unparseable control line %q", i+1, line)
			}
			if listFields[field] {
				m.Control[field] = splitCommaList(value)
			} else {
				m.Control[field] = []string{value}
			}
		case "## conffiles":
			if line == "" || line == "(none)" {
				continue
			}
			m.Conffiles = append(m.Conffiles, line)
		case "## scripts":
			m.Scripts = append(m.Scripts, strings.Fields(line)...)
		case "## files":
			if line == "" {
				continue
			}
			entry, err := parseFileLine(line)
			if err != nil {
				return Manifest{}, fmt.Errorf("manifest line %d: %w", i+1, err)
			}
			m.Files = append(m.Files, entry)
		default:
			if line != "" {
				return Manifest{}, fmt.Errorf("manifest line %d: content %q before any \"## \" section header", i+1, line)
			}
		}
	}

	m.normalize()
	return m, nil
}

// parseFileLine parses one "## files" row: "<type> <mode> <path>" optionally
// followed by " sha256=<hex>" or " -> <target>". Only type and mode are
// guaranteed not to contain a space; path and link target can (a package can
// legitimately ship a path with a space in it), so once type and mode are
// off the front, the rest is split on the *last* " sha256=" or " -> " rather
// than on whitespace.
func parseFileLine(line string) (FileEntry, error) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return FileEntry{}, fmt.Errorf("malformed file entry %q", line)
	}
	entry := FileEntry{Type: parts[0], Mode: parts[1]}
	rest := parts[2]

	switch {
	case strings.LastIndex(rest, " -> ") >= 0:
		i := strings.LastIndex(rest, " -> ")
		entry.Path, entry.LinkTarget = rest[:i], rest[i+len(" -> "):]
	case strings.LastIndex(rest, " sha256=") >= 0:
		i := strings.LastIndex(rest, " sha256=")
		entry.Path, entry.SHA256 = rest[:i], rest[i+len(" sha256="):]
	default:
		entry.Path = rest
	}
	if entry.Path == "" {
		return FileEntry{}, fmt.Errorf("malformed file entry %q", line)
	}
	return entry, nil
}
