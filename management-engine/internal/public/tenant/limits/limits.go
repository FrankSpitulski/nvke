// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package limits defines resource bounds shared by Public API transports and storage.
package limits

import "time"

const (
	// RequestBytes bounds both wire requests and decoded protobuf requests.
	RequestBytes = 64 << 10
	// PageTokenKeyMinBytes is the minimum raw pagination-key length.
	PageTokenKeyMinBytes = 32
	// PageTokenKeyMaxBytes bounds pagination-key configuration files.
	PageTokenKeyMaxBytes = 64
	// RequestTimeout bounds application requests, including receiving their bodies.
	RequestTimeout = 5 * time.Second
	// StorageTimeout bounds a datastore operation even without an ingress deadline.
	StorageTimeout = RequestTimeout
	// ProbeTimeout bounds datastore readiness checks.
	ProbeTimeout = time.Second
)
