// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package debmanifest builds and compares a text "package contract" for an
// nke-kubeadm-distro Debian package: a normalised, deterministic description
// of its control fields, conffiles, maintainer scripts and file listing, in
// the same shape scratchpad/golden/debmanifest.sh produces for the production
// S3 packages. Generate reads the shape from a built .deb; ParseManifest reads
// it back from a committed golden text file; Compare checks the two match up
// to a reviewed allowlist of deviations.
package debmanifest

import (
	"fmt"
	"sort"
	"strings"
)

// binDirs are the payload directories whose regular files carry a sha256 in
// the file listing. Everything else only needs to exist with the right type,
// mode and (for symlinks) target — hashing every one of the ~1600 bundled
// artifact files would make the contract change on every Kubernetes patch
// bump for no benefit, since those are already covered by the lock files.
var binDirs = []string{"/usr/bin/", "/usr/sbin/", "/usr/local/bin/", "/opt/cni/bin/"}

// listFields are the comma-separated control fields that are sorted before
// comparison, so a peer-list reordered by iteration order never causes a
// diff. controlOrder is the order they render in, matching the order dpkg
// itself writes them into DEBIAN/control. Version is deliberately excluded,
// alongside Installed-Size, Maintainer and Description, which readControlFields
// never even requests: it is the packaging revision (DEB_REVISION), not part
// of the contract, and always differs from the S3 package's own revision.
var controlOrder = []string{"Package", "Section", "Priority", "Architecture", "Provides", "Depends", "Conflicts", "Replaces"}

var listFields = map[string]bool{"Provides": true, "Depends": true, "Conflicts": true, "Replaces": true}

// Manifest is the parsed, comparable contract of a .deb package.
type Manifest struct {
	// Control holds the fields named in controlOrder. List fields (see
	// listFields) are pre-split and sorted; scalar fields are single values.
	Control map[string][]string
	// Conffiles is the sorted contents of DEBIAN/conffiles, if any.
	Conffiles []string
	// Scripts is the sorted set of maintainer scripts present in DEBIAN/
	// (preinst, postinst, prerm, postrm).
	Scripts []string
	// Files is the package's file listing, sorted by Path.
	Files []FileEntry
}

// FileEntry is one row of a manifest's file listing: a directory, regular
// file, symlink, hardlink, or device/fifo special file inside the package
// payload.
type FileEntry struct {
	// Type is "d" (directory), "f" (regular file), "l" (symlink), "h"
	// (hardlink), "c" (character device), "b" (block device), or "p"
	// (fifo).
	Type string
	// Mode is the 9-character rwx permission string, e.g. "rwxr-xr-x".
	Mode string
	// Path is the absolute path inside the package, e.g. "/usr/bin/kubeadm".
	// Directories carry a trailing slash.
	Path string
	// SHA256 is set for regular files under one of binDirs; empty otherwise.
	SHA256 string
	// LinkTarget is the symlink or hardlink target; empty otherwise.
	LinkTarget string
}

// isBinPath reports whether path is a regular file under one of binDirs and
// so should carry a sha256 in the manifest.
func isBinPath(path string) bool {
	for _, dir := range binDirs {
		if strings.HasPrefix(path, dir) {
			return true
		}
	}
	return false
}

// normalize sorts every field that must not depend on source order: the
// comma-list control fields, conffiles, scripts, and the file listing.
func (m *Manifest) normalize() {
	for field := range listFields {
		if vals, ok := m.Control[field]; ok {
			sort.Strings(vals)
		}
	}
	sort.Strings(m.Conffiles)
	sort.Strings(m.Scripts)
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
}

// String renders the manifest in the debmanifest.sh text format: a
// "## control" section, a "## conffiles" section, a "## scripts" section, and
// a "## files" section. Given a normalized Manifest, this is deterministic
// regardless of the order fields appeared in the source.
func (m *Manifest) String() string {
	var b strings.Builder

	b.WriteString("## control\n")
	for _, field := range controlOrder {
		vals, ok := m.Control[field]
		if !ok || len(vals) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", field, strings.Join(vals, ", "))
	}

	b.WriteString("## conffiles\n")
	if len(m.Conffiles) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, c := range m.Conffiles {
			fmt.Fprintf(&b, "%s\n", c)
		}
	}

	b.WriteString("## scripts\n")
	fmt.Fprintf(&b, "%s \n", strings.Join(m.Scripts, " "))

	b.WriteString("## files\n")
	for _, f := range m.Files {
		fmt.Fprintf(&b, "%s\n", f.line())
	}

	return b.String()
}

// line renders one "## files" row, matching debmanifest.sh's awk output for
// "d"/"f"/"l" and extending the same shape to "h" (hardlink) and the
// character/block/fifo special types.
func (f FileEntry) line() string {
	switch f.Type {
	case "l", "h":
		return fmt.Sprintf("%s %s %s -> %s", f.Type, f.Mode, f.Path, f.LinkTarget)
	case "f":
		if f.SHA256 != "" {
			return fmt.Sprintf("%s %s %s sha256=%s", f.Type, f.Mode, f.Path, f.SHA256)
		}
		return fmt.Sprintf("%s %s %s", f.Type, f.Mode, f.Path)
	default:
		return fmt.Sprintf("%s %s %s", f.Type, f.Mode, f.Path)
	}
}
