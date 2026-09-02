// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBaseImagesTarballName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"v1.36.3", "k8s-base-container-images_1.36.3_linux_amd64.tgz"},
		{"1.34.2", "k8s-base-container-images_1.34.2_linux_amd64.tgz"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, baseImagesTarballName(tt.in), tt.in)
	}
}

func TestParseImageList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "kubeadm output",
			in: "registry.k8s.io/kube-apiserver:v1.36.3\n" +
				"registry.k8s.io/kube-controller-manager:v1.36.3\n" +
				"registry.k8s.io/kube-scheduler:v1.36.3\n" +
				"registry.k8s.io/kube-proxy:v1.36.3\n" +
				"registry.k8s.io/coredns/coredns:v1.13.1\n" +
				"registry.k8s.io/pause:3.10\n" +
				"registry.k8s.io/etcd:3.6.6-0\n",
			want: []string{
				"registry.k8s.io/kube-apiserver:v1.36.3",
				"registry.k8s.io/kube-controller-manager:v1.36.3",
				"registry.k8s.io/kube-scheduler:v1.36.3",
				"registry.k8s.io/kube-proxy:v1.36.3",
				"registry.k8s.io/coredns/coredns:v1.13.1",
				"registry.k8s.io/pause:3.10",
				"registry.k8s.io/etcd:3.6.6-0",
			},
		},
		{
			name: "warnings and blank lines are dropped",
			in: "W0102 03:04:05.000000    1 common.go:1] WARNING: this is a warning\n" +
				"\n" +
				"registry.k8s.io/pause:3.10\n" +
				"   \n",
			want: []string{"registry.k8s.io/pause:3.10"},
		},
		{
			name: "digest reference",
			in:   "registry.k8s.io/pause@sha256:aaaa\n",
			want: []string{"registry.k8s.io/pause@sha256:aaaa"},
		},
		{
			name: "no images",
			in:   "\n\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseImageList(tt.in))
		})
	}
}
