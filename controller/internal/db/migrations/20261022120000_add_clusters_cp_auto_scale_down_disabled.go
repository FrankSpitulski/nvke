// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

// clusters.cp_auto_scale_down_disabled is a cluster-level control-plane toggle,
// stored as a column alongside cp_desired_count and cp_max_surge rather than in
// the append-only config snapshot. When true the control plane is not scaled
// down automatically on a desiredCount decrease. Default false keeps automatic
// scale-down on, which is the behavior every existing cluster already has.
//
//nolint:gochecknoinits // Bun registers migrations at package initialization.
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, `
			ALTER TABLE clusters
				ADD COLUMN IF NOT EXISTS cp_auto_scale_down_disabled boolean NOT NULL DEFAULT false
		`)
		if err != nil {
			return fmt.Errorf("add clusters.cp_auto_scale_down_disabled: %w", err)
		}
		return nil
	}, func(context.Context, *bun.DB) error {
		return fmt.Errorf("down migration not supported")
	})
}
