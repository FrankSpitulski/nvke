// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dex

import (
	"strings"
	"testing"
)

func TestNewRuntimeRejectsNonHTTPSIssuer(t *testing.T) {
	t.Parallel()
	if _, _, _, _, err := NewRuntime("http://dex.example.test"); err == nil {
		t.Fatal("NewRuntime accepted a non-HTTPS issuer")
	} else if !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("NewRuntime error = %q, want useful HTTPS error", err)
	}
}
