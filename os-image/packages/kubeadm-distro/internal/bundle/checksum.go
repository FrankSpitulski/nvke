// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// FileSHA256 returns the lowercase hex SHA-256 digest of path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WriteSHA256Sidecar hashes path and writes the digest to "<path>.sha256" in the
// bare one-line shape dl.k8s.io publishes.
func WriteSHA256Sidecar(path string) (string, error) {
	digest, err := FileSHA256(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path+".sha256", []byte(digest+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write checksum sidecar for %s: %w", path, err)
	}
	return digest, nil
}

// ParseSHA256Sidecar reads a one-line dl.k8s.io-style sidecar (64 hex chars, optional name suffix).
func ParseSHA256Sidecar(data []byte) (string, error) {
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		hexPart := strings.ToLower(fields[0])
		if len(hexPart) != 64 {
			return "", fmt.Errorf("sidecar digest is not 64 hex characters")
		}
		if _, err := hex.DecodeString(hexPart); err != nil {
			return "", fmt.Errorf("invalid hex digest in sidecar: %w", err)
		}
		return hexPart, nil
	}
	return "", fmt.Errorf("no digest found in checksum sidecar")
}

// VerifyFileAgainstSidecar hashes artifactPath and compares it to the digest in
// sidecarPath, returning the verified digest.
func VerifyFileAgainstSidecar(artifactPath, sidecarPath string) (string, error) {
	sidecar, err := os.ReadFile(sidecarPath)
	if err != nil {
		return "", fmt.Errorf("read sidecar %s: %w", sidecarPath, err)
	}
	want, err := ParseSHA256Sidecar(sidecar)
	if err != nil {
		return "", fmt.Errorf("parse sidecar %s: %w", sidecarPath, err)
	}
	got, err := FileSHA256(artifactPath)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("checksum mismatch for %s: got %s want %s (from %s)", artifactPath, got, want, sidecarPath)
	}
	return got, nil
}

// VerifyManifestDigest checks manifest.json against its manifest.json.sha256 sidecar.
func VerifyManifestDigest(manifestPath string) error {
	sidecarPath := manifestPath + ".sha256"
	st, err := os.Stat(sidecarPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing manifest digest sidecar %s", sidecarPath)
		}
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory", sidecarPath)
	}
	_, err = VerifyFileAgainstSidecar(manifestPath, sidecarPath)
	return err
}
