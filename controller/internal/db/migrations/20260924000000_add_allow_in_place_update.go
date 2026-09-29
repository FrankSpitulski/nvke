// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		// Add allow_in_place_update column with default value of false
		// Default value of false preserves existing behavior: the rollout gate on
		// control-plane addon upgrades stays in effect unless explicitly opted out.
		_, err := db.NewAddColumn().
			Table("clusters").
			ColumnExpr("allow_in_place_update boolean NOT NULL DEFAULT false").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add allow_in_place_update column: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		// Rollback migration: drop allow_in_place_update column
		_, err := db.NewDropColumn().
			Table("clusters").
			Column("allow_in_place_update").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to drop allow_in_place_update column: %w", err)
		}

		return nil
	})
}
