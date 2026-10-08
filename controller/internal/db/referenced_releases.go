// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
)

// ReferencedReleases are the distinct release names that live resources store
// in their os_profile field, split by the kind of release the role selects. A
// control-plane selection names a ClusterRelease and a worker selection a
// NodePoolRelease. Both slices are sorted.
type ReferencedReleases struct {
	// ControlPlane names ClusterReleases.
	ControlPlane []string
	// Worker names NodePoolReleases.
	Worker []string
}

type ReferencedReleaseQueries interface {
	// ListReferencedReleaseNames returns the distinct release names that live
	// clusters and node pools select, excluding anything being deleted.
	ListReferencedReleaseNames(ctx context.Context) (ReferencedReleases, error)
}

// referencedReleasesQuery unions the two places a selectable release name is
// stored: a cluster's control-plane configuration and a node pool.
const (
	referencedReleasesQuery = `
WITH live_clusters AS (
	SELECT id, provider_type, configuration_id
	FROM clusters
	WHERE COALESCE(conditions->'deleting'->>'status', '') <> 'True'
)
SELECT DISTINCT refs.control_plane, refs.name
FROM (
	SELECT TRUE AS control_plane, cpc.config->'nico'->>'os_profile' AS name
	FROM live_clusters c
	JOIN control_plane_configurations cpc ON cpc.id = c.configuration_id
	WHERE c.provider_type = ?
	UNION ALL
	SELECT np.control_plane, np.nico->>'os_profile'
	FROM node_pools np
	JOIN live_clusters c ON c.id = np.cluster_id
	WHERE COALESCE(np.conditions->'deleting'->>'status', '') <> 'True'
) refs
WHERE refs.name IS NOT NULL AND refs.name <> ''
ORDER BY refs.control_plane DESC, refs.name
`
)

func (q *queriesImpl) ListReferencedReleaseNames(ctx context.Context) (ReferencedReleases, error) {
	var rows []struct {
		ControlPlane bool   `bun:"control_plane"`
		Name         string `bun:"name"`
	}
	if err := q.db.NewRaw(referencedReleasesQuery, ProviderTypeNico).Scan(ctx, &rows); err != nil {
		return ReferencedReleases{}, fmt.Errorf("listing referenced releases: %w", err)
	}

	var referenced ReferencedReleases
	for _, row := range rows {
		if row.ControlPlane {
			referenced.ControlPlane = append(referenced.ControlPlane, row.Name)
		} else {
			referenced.Worker = append(referenced.Worker, row.Name)
		}
	}
	return referenced, nil
}
