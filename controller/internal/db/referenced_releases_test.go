// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

type referencedReleasesFixture struct {
	t     *testing.T
	bunDB *bun.DB
	dbImp db.DB
}

func newReferencedReleasesFixture(t *testing.T) *referencedReleasesFixture {
	t.Helper()
	bunDB := testutil.SetupDB(t.Context(), t,
		(*db.Cluster)(nil),
		(*db.ControlPlaneConfiguration)(nil),
		(*db.NodePool)(nil),
		(*db.NodeSet)(nil),
	)
	return &referencedReleasesFixture{t: t, bunDB: bunDB, dbImp: db.NewDB(bunDB)}
}

// cluster inserts a Nico cluster whose control-plane config selects release, or
// no config at all when release is empty.
func (f *referencedReleasesFixture) cluster(release string, deleting bool) db.Cluster {
	f.t.Helper()
	cluster := newValidCluster()
	if deleting {
		cluster.Conditions.Deleting = db.NewCondition(true, "deletion in progress")
	}
	_, err := f.bunDB.NewInsert().Model(&cluster).Exec(f.t.Context())
	require.NoError(f.t, err)
	if release == "" {
		return cluster
	}

	spec := db.ControlPlaneConfigSpec{}
	spec.SetNico(db.NicoControlPlaneProvider{OSProfile: release})
	snapshot, err := f.dbImp.CreateControlPlaneConfiguration(f.t.Context(), &db.ControlPlaneConfiguration{ClusterID: cluster.ID, Config: spec})
	require.NoError(f.t, err)
	_, err = f.bunDB.NewUpdate().
		Model(&db.Cluster{ID: cluster.ID, ControlPlaneConfig: db.ControlPlaneConfig{ConfigurationID: snapshot.ID}}).
		Column("configuration_id").
		WherePK().
		Exec(f.t.Context())
	require.NoError(f.t, err)
	return cluster
}

// pool inserts a node pool of cluster that selects release. A nil provider is a
// CAPI pool.
func (f *referencedReleasesFixture) pool(cluster db.Cluster, controlPlane bool, release *string, deleting bool) db.NodePool {
	f.t.Helper()
	pool := db.NodePool{
		ID:                   uuid.New(),
		ClusterID:            cluster.ID,
		Name:                 "pool-" + uuid.NewString(),
		NodeNamePrefix:       "n",
		SystemReservedMemory: "1Gi",
		ControlPlane:         controlPlane,
		NodeLabels:           map[string]string{},
		NodeTaints:           []db.NodeTaint{},
	}
	if deleting {
		pool.Conditions.Deleting = db.NewCondition(true, "deletion in progress")
	}
	if release != nil {
		pool.SetNico(db.NicoNodePoolProvider{OSProfile: *release})
	}
	_, err := f.bunDB.NewInsert().Model(&pool).Exec(f.t.Context())
	require.NoError(f.t, err)
	return pool
}

func (f *referencedReleasesFixture) set(pool db.NodePool, release string) {
	f.t.Helper()
	set := db.NodeSet{ID: uuid.New(), NodePoolID: pool.ID}
	set.SetNico(db.NicoNodePoolProvider{OSProfile: release})
	_, err := f.bunDB.NewInsert().Model(&set).Exec(f.t.Context())
	require.NoError(f.t, err)
}

func TestListReferencedReleaseNames(t *testing.T) {
	str := func(s string) *string { return &s }

	t.Run("an empty database references nothing", func(t *testing.T) {
		f := newReferencedReleasesFixture(t)

		got, err := f.dbImp.ListReferencedReleaseNames(f.t.Context())

		require.NoError(t, err)
		assert.Empty(t, got.ControlPlane)
		assert.Empty(t, got.Worker)
	})

	t.Run("names are split by the role of the selecting resource, distinct and sorted", func(t *testing.T) {
		f := newReferencedReleasesFixture(t)
		cluster := f.cluster("cp-cluster", false)
		f.pool(cluster, true, str("cp-pool"), false)
		f.pool(cluster, false, str("w-pool"), false)
		f.pool(cluster, false, str("w-other"), false)
		// A second cluster that picks the same names adds nothing new.
		other := f.cluster("cp-cluster", false)
		f.pool(other, false, str("w-pool"), false)

		got, err := f.dbImp.ListReferencedReleaseNames(f.t.Context())

		require.NoError(t, err)
		assert.Equal(t, []string{"cp-cluster", "cp-pool"}, got.ControlPlane)
		assert.Equal(t, []string{"w-other", "w-pool"}, got.Worker)
	})

	t.Run("a superseded node set does not keep its release referenced", func(t *testing.T) {
		f := newReferencedReleasesFixture(t)
		cluster := f.cluster("", false)
		// Mid-rollout: the pool already selects the new release while the sets
		// of the previous configuration still name the old one. Only the pool's
		// selection can be provisioned, so only it is referenced.
		pool := f.pool(cluster, false, str("w-new"), false)
		f.set(pool, "w-old")

		got, err := f.dbImp.ListReferencedReleaseNames(f.t.Context())

		require.NoError(t, err)
		assert.Empty(t, got.ControlPlane)
		assert.Equal(t, []string{"w-new"}, got.Worker)
	})

	t.Run("a deleting cluster and its pools are left out", func(t *testing.T) {
		f := newReferencedReleasesFixture(t)
		deleting := f.cluster("cp-gone", true)
		// A cluster delete marks only the cluster, so its pools look alive.
		f.pool(deleting, true, str("cp-pool-gone"), false)
		f.pool(deleting, false, str("w-pool-gone"), false)
		live := f.cluster("cp-live", false)
		f.pool(live, false, str("w-live"), false)

		got, err := f.dbImp.ListReferencedReleaseNames(f.t.Context())

		require.NoError(t, err)
		assert.Equal(t, []string{"cp-live"}, got.ControlPlane)
		assert.Equal(t, []string{"w-live"}, got.Worker)
	})

	t.Run("a deleting pool is left out of a live cluster", func(t *testing.T) {
		f := newReferencedReleasesFixture(t)
		cluster := f.cluster("", false)
		f.pool(cluster, false, str("w-gone"), true)
		f.pool(cluster, false, str("w-live"), false)

		got, err := f.dbImp.ListReferencedReleaseNames(f.t.Context())

		require.NoError(t, err)
		assert.Empty(t, got.ControlPlane)
		assert.Equal(t, []string{"w-live"}, got.Worker)
	})

	t.Run("CAPI pools and empty names select nothing", func(t *testing.T) {
		f := newReferencedReleasesFixture(t)
		cluster := f.cluster("", false)
		f.pool(cluster, false, nil, false)
		f.pool(cluster, true, str(""), false)

		got, err := f.dbImp.ListReferencedReleaseNames(f.t.Context())

		require.NoError(t, err)
		assert.Empty(t, got.ControlPlane)
		assert.Empty(t, got.Worker)
	})
}
