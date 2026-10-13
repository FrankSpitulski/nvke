// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sbomName derives the SBOM file name the release publishes beside an image:
// nke-node-fde-ubuntu-24.04.5-amd64.qcow2 -> ...-amd64.spdx.json.
func sbomName(imageName string) string {
	return strings.TrimSuffix(imageName, ".qcow2") + ".spdx.json"
}

// spdxDocument is the subset of SPDX JSON the validator reads.
type spdxDocument struct {
	Packages []struct {
		Name         string `json:"name"`
		VersionInfo  string `json:"versionInfo"`
		ExternalRefs []struct {
			ReferenceType    string `json:"referenceType"`
			ReferenceLocator string `json:"referenceLocator"`
		} `json:"externalRefs"`
	} `json:"packages"`
}

// LoadSBOM reads the artifact's SPDX SBOM from the bundle directory and
// returns its deb package set (name -> version). Before parsing, the file is
// verified against the digest the (attested) checksums record for it — the
// SBOM is evidence, so an SBOM the checksums do not vouch for is an error,
// not a fallback. A missing SBOM file returns (nil, nil): the caller reports
// that as a visible skip.
func (b *Bundle) LoadSBOM(imageName string) (map[string]string, error) {
	name := sbomName(imageName)
	raw, err := os.ReadFile(filepath.Join(b.Dir, name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading SBOM %s: %w", name, err)
	}
	want, ok := b.Checksums[name]
	if !ok {
		return nil, fmt.Errorf("SBOM %s is not listed in the release checksums", name)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("SBOM %s digest %s does not match the checksums entry %s", name, got, want)
	}

	var doc spdxDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing SBOM %s: %w", name, err)
	}
	pkgs := make(map[string]string)
	for _, p := range doc.Packages {
		if !isDebPackage(p.Name, purlOf(p.ExternalRefs)) {
			continue
		}
		if p.Name == "" || p.VersionInfo == "" {
			continue
		}
		pkgs[p.Name] = p.VersionInfo
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("SBOM %s holds no deb packages", name)
	}
	return pkgs, nil
}

func purlOf(refs []struct {
	ReferenceType    string `json:"referenceType"`
	ReferenceLocator string `json:"referenceLocator"`
},
) string {
	for _, r := range refs {
		if r.ReferenceType == "purl" {
			return r.ReferenceLocator
		}
	}
	return ""
}

// isDebPackage keeps SPDX entries that describe installed deb packages and
// drops everything else, including the document's root entry for the image
// itself. Strictly purl-based: calibrated against the real v0.2.0-rc.1 SBOM,
// where 729 of 730 entries carry a pkg:deb purl and the one that does not is
// the image root.
func isDebPackage(_, purl string) bool {
	return strings.HasPrefix(purl, "pkg:deb/")
}
