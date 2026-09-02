// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"
	"strings"
)

// pins are the third-party release tags bundled alongside one Kubernetes patch.
type pins struct {
	Containerd string
	Runc       string
	Helm       string
	CriTools   string
}

// minorPins is the pin table, one row per supported Kubernetes minor.
//
// containerd follows the project's "Kubernetes support" matrix, pinned to the
// newest patch published on the line it lists:
// https://github.com/containerd/containerd/blob/main/RELEASES.md#kubernetes-support
//
// runc is not enumerated there, so these are pragmatic pairings with each
// containerd line, from https://github.com/opencontainers/runc/releases.
//
// Helm follows https://helm.sh/docs/v3/topics/version_skew/, newest patch on
// each supported line. Kubernetes 1.36 uses Helm v4.x because the published
// skew table has not caught up.
//
// There is deliberately no fallback row. A Kubernetes minor with no entry here
// is an error, so rotating K8S_VERSIONS forces someone to read both upstream
// matrices rather than inheriting whatever the newest line happened to be.
var minorPins = map[int]pins{
	36: {Containerd: "v2.3.0", Runc: "v1.4.2", Helm: "v4.1.4"},
	35: {Containerd: "v2.2.3", Runc: "v1.3.5", Helm: "v3.20.2"},
	34: {Containerd: "v2.1.7", Runc: "v1.3.5", Helm: "v3.20.2"},
	33: {Containerd: "v2.1.7", Runc: "v1.2.9", Helm: "v3.20.2"},
	32: {Containerd: "v2.1.7", Runc: "v1.2.9", Helm: "v3.20.2"},
	31: {Containerd: "v1.7.31", Runc: "v1.2.9", Helm: "v3.19.5"},
	30: {Containerd: "v1.7.31", Runc: "v1.2.9", Helm: "v3.18.6"},
	29: {Containerd: "v1.7.31", Runc: "v1.2.9", Helm: "v3.17.4"},
}

// cniPluginsVersion is a single pin for every row; the CNI plugins are not
// skewed against the Kubernetes minor.
const cniPluginsVersion = "v1.6.0"

// pinsFor returns the containerd, runc, Helm and cri-tools tags bundled with a
// Kubernetes release. Only the minor participates in the choice; an unlisted
// minor is an error rather than a guess.
func pinsFor(k8sVersion string) (pins, error) {
	major, minor, ok := k8sMajorMinor(k8sVersion)
	if !ok {
		return pins{}, fmt.Errorf("cannot read a major.minor from Kubernetes version %q", k8sVersion)
	}
	p, ok := minorPins[minor]
	if !ok || major != 1 {
		return pins{}, fmt.Errorf(
			"no containerd, runc and helm pins for Kubernetes %d.%d: add a row to minorPins in cmd/kubeadm-bundle/pins.go, "+
				"choosing containerd from its RELEASES.md Kubernetes support matrix and helm from its version skew policy",
			major, minor)
	}
	p.CriTools = criToolsRelease(k8sVersion)
	return p, nil
}

// k8sMajorMinor parses the major and minor of a Kubernetes version (v1.36.3 or 1.36.3).
func k8sMajorMinor(k8sVersion string) (major, minor int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(k8sVersion), "v"), ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// criToolsRelease maps a Kubernetes patch to the cri-tools release for its minor;
// cri-tools only tags v1.MM.0.
func criToolsRelease(k8sVersion string) string {
	major, minor, ok := k8sMajorMinor(k8sVersion)
	if !ok {
		return ""
	}
	return fmt.Sprintf("v%d.%d.0", major, minor)
}

// parseVersions splits a comma- or whitespace-separated Kubernetes version list
// and normalises every entry to the leading-v form the download URLs use.
func parseVersions(s string) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		v, err := normalizeK8sVersion(f)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Kubernetes versions given")
	}
	return out, nil
}

// normalizeK8sVersion returns a full vX.Y.Z version, rejecting anything else.
func normalizeK8sVersion(v string) (string, error) {
	v = strings.TrimSpace(v)
	bare := strings.TrimPrefix(v, "v")
	parts := strings.Split(bare, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid Kubernetes version %q: want vX.Y.Z", v)
	}
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("invalid Kubernetes version %q: want vX.Y.Z", v)
		}
		if _, err := strconv.Atoi(p); err != nil {
			return "", fmt.Errorf("invalid Kubernetes version %q: %w", v, err)
		}
	}
	return "v" + bare, nil
}
