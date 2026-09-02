// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVersions(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"spaces", "v1.36.3 v1.35.3 v1.34.2", []string{"v1.36.3", "v1.35.3", "v1.34.2"}, false},
		{"commas", "v1.36.3,v1.35.3", []string{"v1.36.3", "v1.35.3"}, false},
		{"mixed and untrimmed", " 1.36.3 , v1.35.3\n", []string{"v1.36.3", "v1.35.3"}, false},
		{"deduplicated", "v1.36.3 1.36.3", []string{"v1.36.3"}, false},
		{"empty", "  ", nil, true},
		{"minor only", "v1.36", nil, true},
		{"not numeric", "v1.36.x", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVersions(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPinsFor(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		containerd string
		runc       string
		helm       string
		criTools   string
	}{
		{"1.36", "v1.36.3", "v2.3.0", "v1.4.2", "v4.1.4", "v1.36.0"},
		{"1.35", "v1.35.3", "v2.2.3", "v1.3.5", "v3.20.2", "v1.35.0"},
		{"1.34", "v1.34.2", "v2.1.7", "v1.3.5", "v3.20.2", "v1.34.0"},
		{"1.33", "v1.33.6", "v2.1.7", "v1.2.9", "v3.20.2", "v1.33.0"},
		{"1.32", "v1.32.9", "v2.1.7", "v1.2.9", "v3.20.2", "v1.32.0"},
		{"1.31", "v1.31.12", "v1.7.31", "v1.2.9", "v3.19.5", "v1.31.0"},
		{"1.30", "v1.30.1", "v1.7.31", "v1.2.9", "v3.18.6", "v1.30.0"},
		{"1.29", "v1.29.1", "v1.7.31", "v1.2.9", "v3.17.4", "v1.29.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := pinsFor(tt.version)
			require.NoError(t, err)
			assert.Equal(t, tt.containerd, p.Containerd, "containerd")
			assert.Equal(t, tt.runc, p.Runc, "runc")
			assert.Equal(t, tt.helm, p.Helm, "helm")
			assert.Equal(t, tt.criTools, p.CriTools, "cri-tools")
		})
	}
}

func TestPinsForUnknownMinorIsAnError(t *testing.T) {
	tests := []struct {
		name      string
		version   string
		errSubstr string
	}{
		{"a minor newer than the table", "v1.37.0", "no containerd, runc and helm pins for Kubernetes 1.37"},
		{"a minor older than the table", "v1.28.0", "no containerd, runc and helm pins for Kubernetes 1.28"},
		{"a major other than 1", "v2.0.0", "no containerd, runc and helm pins for Kubernetes 2.0"},
		{"not a version at all", "not-a-version", "cannot read a major.minor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pinsFor(tt.version)
			require.ErrorContains(t, err, tt.errSubstr)
			// The message has to say what to do about it.
			if strings.Contains(err.Error(), "no containerd") {
				assert.Contains(t, err.Error(), "add a row to minorPins")
			}
		})
	}
}

func TestComponentURLs(t *testing.T) {
	p, err := pinsFor("v1.36.3")
	require.NoError(t, err)
	urls := componentURLs("v1.36.3", p)
	assert.Equal(t, "https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm", urls["kubeadm"])
	assert.Equal(t, "https://get.helm.sh/helm-v4.1.4-linux-amd64.tar.gz", urls["helm"])
	assert.Equal(t, "https://github.com/opencontainers/runc/releases/download/v1.4.2/runc.amd64", urls["runc"])
	assert.Equal(t,
		"https://github.com/containerd/containerd/releases/download/v2.3.0/containerd-2.3.0-linux-amd64.tar.gz",
		urls["containerd"])
	assert.Equal(t,
		"https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.36.0/crictl-v1.36.0-linux-amd64.tar.gz",
		urls["crictl"])
	assert.Equal(t,
		"https://github.com/containernetworking/plugins/releases/download/v1.6.0/cni-plugins-linux-amd64-v1.6.0.tgz",
		urls["cni-plugins"])
}
