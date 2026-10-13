// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package static implements the T-static check tier: facts that need no image
// introspection — digests, release-checksums membership, and artifact-name
// identity. The inputs come from the release's attested checksums.txt, whose
// own signature is verified by pkg/bundle (PR2); until then the caller decides
// how much to trust the file it passes in.
package static

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

// Impls returns the static-tier implementations keyed by check ID, for
// checks.NewEngine.
func Impls() map[string]checks.Func {
	return map[string]checks.Func{
		"INT-001":    checkImageDigest,
		"INT-002":    checkEvidenceVerified,
		"COMPAT-001": checkArtifactIdentity,
	}
}

// HashFile computes the bare-hex sha256 of a file, streaming.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ParseChecksums reads a sha256sum-format file (`<64 hex>  <name>` per line,
// the format os-image-artifact.yml publishes) into a name → digest map. A
// malformed line is an error: a checksums file that parses partially would
// turn a missing artifact into a silent skip.
func ParseChecksums(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening checksums: %w", err)
	}
	defer f.Close()
	sums := make(map[string]string)
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		digest, name, ok := strings.Cut(text, "  ")
		name = strings.TrimPrefix(name, "*") // sha256sum binary-mode marker
		if !ok || !isHexDigest(digest) || name == "" {
			return nil, fmt.Errorf("checksums %s line %d: not sha256sum format", path, line)
		}
		if prev, dup := sums[name]; dup && prev != digest {
			return nil, fmt.Errorf("checksums %s line %d: conflicting digests for %s", path, line, name)
		}
		sums[name] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading checksums %s: %w", path, err)
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("checksums %s: no entries", path)
	}
	return sums, nil
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// artifactName matches the release naming scheme the os-image workflow
// publishes: nvke-node-fde-ubuntu-24.04.5-amd64.qcow2, and the nke-node-*
// spelling that releases up to v0.2.0 carry (the nke→nvke rename, #1548).
var artifactName = regexp.MustCompile(`^nv?ke-node-(fde|non-fde)-ubuntu-([0-9][0-9.]*)-(amd64|arm64)\.qcow2$`)

// Identity is what an artifact's release name declares about it.
type Identity struct {
	Variant checks.Variant
	Ubuntu  string
	Arch    string
}

// IdentityFromName parses an artifact release name into its declared identity.
func IdentityFromName(name string) (Identity, error) {
	m := artifactName.FindStringSubmatch(name)
	if m == nil {
		return Identity{}, fmt.Errorf("artifact name %q does not match the release naming scheme", name)
	}
	return Identity{Variant: checks.Variant(m[1]), Ubuntu: m[2], Arch: m[3]}, nil
}

// checkImageDigest (INT-001): the image file's sha256 equals the digest the
// release checksums record under the target's artifact name.
func checkImageDigest(_ context.Context, t checks.Target, in checks.Inputs) (checks.Outcome, error) {
	want, ok := in.Checksums[t.ImageName]
	if !ok {
		return checks.Outcome{
			Status:   checks.StatusFail,
			Evidence: fmt.Sprintf("artifact %s not listed in release checksums", t.ImageName),
		}, nil
	}
	if in.ImageSHA256 != want {
		return checks.Outcome{
			Status:   checks.StatusFail,
			Evidence: fmt.Sprintf("image sha256 %s != recorded %s for %s", in.ImageSHA256, want, t.ImageName),
		}, nil
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("sha256 %s matches release checksums entry %s", want, t.ImageName),
	}, nil
}

// checkArtifactIdentity (COMPAT-001): the variant, Ubuntu release and
// architecture the artifact's name declares are the ones being validated
// against — a mismatch means the wrong image is under test.
func checkArtifactIdentity(_ context.Context, t checks.Target, _ checks.Inputs) (checks.Outcome, error) {
	id, err := IdentityFromName(t.ImageName)
	if err != nil {
		return checks.Outcome{}, err
	}
	var mismatches []string
	if id.Variant != t.Variant {
		mismatches = append(mismatches, fmt.Sprintf("variant %s != target %s", id.Variant, t.Variant))
	}
	if id.Arch != t.Arch {
		mismatches = append(mismatches, fmt.Sprintf("arch %s != target %s", id.Arch, t.Arch))
	}
	if id.Ubuntu != t.Ubuntu {
		mismatches = append(mismatches, fmt.Sprintf("ubuntu %s != target %s", id.Ubuntu, t.Ubuntu))
	}
	if len(mismatches) > 0 {
		return checks.Outcome{Status: checks.StatusFail, Evidence: strings.Join(mismatches, "; ")}, nil
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("name declares %s/%s/ubuntu-%s, matching the target", id.Variant, id.Arch, id.Ubuntu),
	}, nil
}

// checkEvidenceVerified (INT-002): the release evidence attestation verified
// offline against the pinned trusted root, and its subject is this bundle's
// checksums.txt. A digest-only run (no bundle) is an explicit caller choice
// and reports a skip with the reason — visible in the report, never a pass.
func checkEvidenceVerified(_ context.Context, _ checks.Target, in checks.Inputs) (checks.Outcome, error) {
	ev := in.Evidence
	if !ev.Checked {
		return checks.Outcome{
			Status:   checks.StatusSkip,
			Evidence: "evidence verification not attempted: digest-only (--checksums) mode",
		}, nil
	}
	if ev.Err != nil {
		return checks.Outcome{Status: checks.StatusFail, Evidence: ev.Err.Error()}, nil
	}
	return checks.Outcome{
		Status:   checks.StatusPass,
		Evidence: fmt.Sprintf("attestation verified; signer %s", ev.SignerSAN),
	}, nil
}
