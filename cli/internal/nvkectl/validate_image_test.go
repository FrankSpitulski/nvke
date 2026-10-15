// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runValidateImageCmd drives the real command tree, so the flags, their
// markers and the output plumbing are exercised rather than just the runner
// beneath them.
func runValidateImageCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewRootCmd("test", "none", "clean", "now")
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// imageFixture writes an image file plus a checksums.txt recording its digest
// (or a tampered one) and returns the common argument list for digest-only
// validation. The static tier is content-agnostic, so no real qcow2 is needed.
func imageFixture(t *testing.T, tampered bool) []string {
	t.Helper()
	dir := t.TempDir()
	const artifact = "nvke-node-fde-ubuntu-26.04.1-amd64.qcow2"
	content := []byte("image bytes for the command test")
	image := filepath.Join(dir, artifact)
	if err := os.WriteFile(image, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	if tampered {
		digest = strings.Repeat("0", len(digest))
	}
	checksums := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(checksums, []byte(fmt.Sprintf("%s  %s\n", digest, artifact)), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"validate", "image",
		"--image", image,
		"--checksums", checksums,
		"--checks", filepath.Join("..", "..", "..", "validate", "checks.yaml"),
		"--target-variant", "fde",
		"--target-arch", "amd64",
		"--target-ubuntu", "26.04.1",
		"--inspect", "off",
	}
}

func TestValidateImagePassesAndPrintsTheReport(t *testing.T) {
	stdout, _, err := runValidateImageCmd(t, imageFixture(t, false)...)
	if err != nil {
		t.Fatalf("validate image: %v", err)
	}
	for _, want := range []string{"INT-001", "COMPAT-001", "check set"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report on stdout misses %q:\n%s", want, stdout)
		}
	}
}

func TestValidateImageFailureIsAnErrorNamingTheReason(t *testing.T) {
	stdout, _, err := runValidateImageCmd(t, imageFixture(t, true)...)
	if err == nil {
		t.Fatal("a tampered digest must fail the command")
	}
	if !strings.Contains(err.Error(), "blocking check failed") {
		t.Errorf("error does not carry the blocking-fail distinction: %v", err)
	}
	if !strings.Contains(stdout, "FAIL") {
		t.Errorf("the report must still reach stdout on failure:\n%s", stdout)
	}
}

func TestValidateImageEvidenceFlagsAreExclusiveAndRequired(t *testing.T) {
	args := imageFixture(t, false)
	_, _, err := runValidateImageCmd(t, append(args, "--bundle", t.TempDir())...)
	if err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Errorf("bundle and checksums together must be rejected by the flag marker, got: %v", err)
	}

	var withoutEvidence []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--checksums" {
			i++
			continue
		}
		withoutEvidence = append(withoutEvidence, args[i])
	}
	_, _, err = runValidateImageCmd(t, withoutEvidence...)
	if err == nil || !strings.Contains(err.Error(), "at least one of the flags") {
		t.Errorf("missing evidence source must be rejected by the flag marker, got: %v", err)
	}
}
