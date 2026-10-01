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
		// Keeps the 20260202120000 slot: the previous file here also backfilled
		// these columns from osProfiles/metadata.yaml, which CWE no longer reads.
		// The slot has to stay below 20260403140000, which selects
		// node_pools.kubernetes_version. bun keys on the timestamp alone, so
		// databases that applied the old file skip this one.
		for _, table := range []string{"node_pools", "node_sets"} {
			if _, err := db.NewAddColumn().
				Table(table).
				ColumnExpr("kubernetes_version text NOT NULL DEFAULT ''").
				IfNotExists().
				Exec(ctx); err != nil {
				return fmt.Errorf("failed to add %s.kubernetes_version column: %w", table, err)
			}
		}

		return nil
	}, func(_ context.Context, _ *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
