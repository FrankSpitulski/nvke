// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package debmanifest

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// modeStringPattern matches the 9-character permission strings permString
// produces: r/w/x-or-"-" in the plain positions, plus the setuid/setgid
// (s/S) and sticky (t/T) variants in the owner-execute, group-execute and
// other-execute positions respectively.
var modeStringPattern = regexp.MustCompile(`^[r-][w-][xsS-][r-][w-][xsS-][r-][w-][xtT-]$`)

// Deviation is one reviewed, intentional difference between the golden
// manifest and the package this build produces. Every deviation carries the
// reason it was allowed, taken from the "# ..." comment on its line.
type Deviation struct {
	// Kind is "removed", "added", "mode", "field", "scripts", or
	// "conffiles".
	Kind string
	// Field is set only for Kind == "field": the control field the
	// deviation adjusts (e.g. "Depends").
	Field string
	// Op is the operator for Kind == "field", "scripts" or "conffiles":
	// "+=" to add values, "-=" to remove them, or "=" to replace the list
	// (or scalar field) outright.
	Op string
	// Values holds the file path for "removed"/"added"; [path, newmode]
	// for "mode"; or the list of values for "field"/"scripts"/"conffiles".
	Values []string
	// Reason is the text after " #" on the deviation's line.
	Reason string
	// Optional is set by a leading "optional " on the directive line: the
	// deviation is applied when it matches, same as any other, but is not
	// reported as unused when it matches nothing. See the "optional" note
	// in ParseDeviations's doc comment.
	Optional bool

	// line is the 1-based source line number, for error messages.
	line int
	// raw is the original line, for error messages.
	raw string
}

// ParseDeviations parses a testdata/golden/deviations.txt file. Blank lines
// and lines whose first non-whitespace character is "#" are pure comments
// and are ignored. Every other line is a directive and MUST carry a " #"
// (space, then hash) reason separator later on the line — required rather
// than a bare "#" so that a "#" appearing inside a path is not mistaken for
// the start of the reason; ParseDeviations rejects any directive line
// without one, and rejects an empty reason.
//
// Directive grammar, one per line. Any directive may take an "optional "
// prefix — see the note below the table.
//
//	removed <path-or-glob>                  # reason
//	added <path-or-glob>                    # reason
//	mode <path-or-glob> <newmode>            # reason
//	field <Field> += <v1>, <v2>, ...         # reason
//	field <Field> -= <v1>, <v2>, ...         # reason
//	field <Field> = <v1>, <v2>, ...          # reason
//	scripts += <name> ...                   # reason
//	scripts -= <name> ...                   # reason
//	scripts = <name> ...                    # reason
//	conffiles += <path> ...                 # reason
//	conffiles -= <path> ...                 # reason
//	conffiles = <path> ...                  # reason
//	optional <any of the above>             # reason (must say which version)
//
// "field" only allows "+=" or "-=" on the four list fields (Provides,
// Depends, Conflicts, Replaces); a scalar field (Package, Section, Priority,
// Architecture — see scalarFields) only allows "=", and ParseDeviations
// rejects "+="/"-=" on one at parse time rather than silently doing nothing.
//
// The <path-or-glob> argument to "removed", "added" and "mode" is a
// path.Match pattern: "*" matches any run of characters within one path
// segment and "?" matches one such character, but neither ever crosses a
// "/" — no "/**" or other cross-segment wildcard is needed or supported. A
// literal path with no metacharacters matches only itself, so existing
// literal directives keep working unchanged. This is for drift whose exact
// path is version-specific — e.g.
// "/usr/lib/nke/k8s-matrix/v1.36.3/amd64/containerd-2.3.0-linux-amd64.tar.gz"
// vs the "v1.34.2" row's own containerd release — where one glob line
// (`removed /usr/lib/nke/k8s-matrix/*/amd64/containerd-*-linux-amd64.tar.gz`)
// covers every Kubernetes patch instead of needing one literal line per
// version. A glob directive counts as used once it matches at least one
// entry in the relevant comparison (golden for "removed"/"mode", actual for
// "added"), even when it also matches several.
//
// A leading "optional " on any directive (e.g. "optional added <glob>  #
// reason") marks it not-unused when it matches nothing — it still applies
// normally when it does match. This is for per-version asymmetries a glob
// alone cannot express: for example the production 1.36.3 package ships no
// k8s-base-container-images_*.tgz while 1.34.2 and 1.35.3 do, so
// "added .../k8s-base-container-images_*.tgz" is needed for 1.36.3's
// comparison but would be a stale-looking no-op for the other two under
// strict mode. "optional" is for exactly this kind of asymmetry, not a
// general escape hatch from allowlist rot — every "optional" line's reason
// must say which version(s) it exists for.
func ParseDeviations(text string) ([]*Deviation, error) {
	var devs []*Deviation
	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		idx := strings.Index(trimmed, " #")
		if idx < 0 {
			return nil, fmt.Errorf("deviations.txt:%d: missing \" #\" reason separator: %q", lineNo, raw)
		}
		directive := strings.TrimSpace(trimmed[:idx])
		reason := strings.TrimSpace(trimmed[idx+len(" #"):])
		if reason == "" {
			return nil, fmt.Errorf("deviations.txt:%d: empty reason: %q", lineNo, raw)
		}

		dev, err := parseDirective(directive)
		if err != nil {
			return nil, fmt.Errorf("deviations.txt:%d: %w", lineNo, err)
		}
		dev.Reason = reason
		dev.line = lineNo
		dev.raw = raw
		devs = append(devs, dev)
	}
	return devs, nil
}

// validateGlob reports a syntax error in pattern early, at parse time,
// rather than letting a malformed glob silently match nothing later in
// Compare (see pathGlobMatches in compare.go).
func validateGlob(pattern string) error {
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("malformed glob: %w", err)
	}
	return nil
}

func parseDirective(directive string) (*Deviation, error) {
	optional := false
	if rest, ok := strings.CutPrefix(directive, "optional "); ok {
		optional = true
		directive = strings.TrimSpace(rest)
	}

	fields := strings.Fields(directive)
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty directive")
	}

	dev, err := parseKindDirective(fields, directive)
	if err != nil {
		return nil, err
	}
	dev.Optional = optional
	return dev, nil
}

// parseKindDirective parses everything after an optional "optional " prefix
// has already been stripped: the "<kind> ..." directive itself.
func parseKindDirective(fields []string, directive string) (*Deviation, error) {
	switch fields[0] {
	case "removed", "added":
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s wants exactly one path (or glob), got %q", fields[0], directive)
		}
		if err := validateGlob(fields[1]); err != nil {
			return nil, fmt.Errorf("%s %s: %w", fields[0], fields[1], err)
		}
		return &Deviation{Kind: fields[0], Values: []string{fields[1]}}, nil

	case "mode":
		if len(fields) != 3 {
			return nil, fmt.Errorf("mode wants \"mode <path> <newmode>\", got %q", directive)
		}
		if err := validateGlob(fields[1]); err != nil {
			return nil, fmt.Errorf("mode %s: %w", fields[1], err)
		}
		if !modeStringPattern.MatchString(fields[2]) {
			return nil, fmt.Errorf("mode %s: %q is not a 9-character permission string (e.g. \"rwxr-xr-x\")", fields[1], fields[2])
		}
		return &Deviation{Kind: "mode", Values: []string{fields[1], fields[2]}}, nil

	case "field":
		if len(fields) < 3 {
			return nil, fmt.Errorf("field directive wants \"field <Name> <op> <values>\", got %q", directive)
		}
		field, op := fields[1], fields[2]
		if !listFields[field] && !scalarFields[field] {
			return nil, fmt.Errorf("field %s is not part of the contract; expected one of Package, Section, Priority, Architecture, Provides, Depends, Conflicts, Replaces", field)
		}
		if op != "+=" && op != "-=" && op != "=" {
			return nil, fmt.Errorf("field %s: unknown operator %q", field, op)
		}
		if scalarFields[field] && op != "=" {
			return nil, fmt.Errorf("field %s is a scalar field; only \"=\" is allowed, got %q", field, op)
		}
		rest := strings.TrimSpace(strings.SplitN(directive, op, 2)[1])
		values := splitCommaList(rest)
		if len(values) == 0 {
			return nil, fmt.Errorf("field %s %s: no values given", field, op)
		}
		return &Deviation{Kind: "field", Field: field, Op: op, Values: values}, nil

	case "scripts", "conffiles":
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s directive wants an operator and at least one value, got %q", fields[0], directive)
		}
		op := fields[1]
		if op != "+=" && op != "-=" && op != "=" {
			return nil, fmt.Errorf("%s: unknown operator %q", fields[0], op)
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("%s %s: no values given", fields[0], op)
		}
		return &Deviation{Kind: fields[0], Op: op, Values: fields[2:]}, nil

	default:
		return nil, fmt.Errorf("unknown deviation kind %q", fields[0])
	}
}
