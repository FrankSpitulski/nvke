// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package k8s

import "k8s.io/client-go/rest"

func ConfigureDynamicClientRESTConfigForTest(cfg *rest.Config) {
	configureDynamicClientRESTConfig(cfg)
}

func EncryptRewriteWorkersForTest() int {
	return encryptRewriteWorkers
}
