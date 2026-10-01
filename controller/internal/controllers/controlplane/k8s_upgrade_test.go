// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	enginev1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestUpdateNodePoolKubeVersion_EmptyOSProfile(t *testing.T) {
	t.Parallel()

	nodePoolID := uuid.New()
	storedPool := db.NodePool{
		ID:                nodePoolID,
		KubernetesVersion: "1.34.2",
		Nico: &db.NicoNodePoolProvider{
			OSProfile: "",
		},
	}
	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(storedPool, nil)
	mockTx.On("UpdateNodePool", mock.Anything, mock.MatchedBy(func(pool *db.NodePool) bool {
		nico, err := pool.NicoProvider()
		return err == nil &&
			pool.ID == nodePoolID &&
			pool.KubernetesVersion == "1.35.0" &&
			nico.OSProfile == ""
	})).Return(db.NodePool{}, nil)

	r := &reconciler{db: mockTx, logger: slog.Default()}
	err := r.updateNodePoolKubeVersion(
		context.Background(),
		db.Cluster{KubernetesVersion: "1.35.0", ProviderType: db.ProviderTypeCAPI},
		nodePool{NodePool: storedPool},
	)

	require.NoError(t, err)
	mockTx.AssertExpectations(t)
}

func TestSelectUpgradeRelease(t *testing.T) {
	releases := releaseFixtures{
		newReleaseFixture("nke-1.31.9-260101", "1.31.9", "amd64", "", "2026-01-01", false, false),
		newReleaseFixture("nke-1.31.9-260115", "1.31.9", "amd64", "", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260120", "1.31.9", "amd64", "", "2026-01-20", false, false),
		newReleaseFixture("nke-1.31.9-260115-arm64", "1.31.9", "arm64", "arm64", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260115-ovn", "1.31.9", "amd64", "ovn", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260120-ovn", "1.31.9", "amd64", "ovn", "2026-01-20", false, false),
		newReleaseFixture("nke-1.31.9-260110-osmo", "1.31.9", "amd64", "osmo", "2026-01-10", false, false),
		newReleaseFixture("nke-1.30.5-260115", "1.30.5", "amd64", "", "2026-01-15", false, false),
	}

	tests := []struct {
		name               string
		k8sVersion         string
		currentProfileName string
		expectedProfile    string
		expectError        bool
		errorContains      string
	}{
		{
			name:               "no current profile - no default to amd64/22.04, returns error (no matching profiles)",
			k8sVersion:         "1.31.9",
			currentProfileName: "",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "no OSProfile found for Kubernetes version 1.31.9",
		},
		{
			name:               "preserve amd64 architecture",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.30.5-260115",
			expectedProfile:    "nke-1.31.9-260120",
			expectError:        false,
		},
		{
			name:               "preserve arm64 architecture",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260115-arm64",
			expectedProfile:    "nke-1.31.9-260115-arm64",
			expectError:        false,
		},
		{
			name:               "preserve ovn variant",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260115-ovn",
			expectedProfile:    "nke-1.31.9-260120-ovn",
			expectError:        false,
		},
		{
			name:               "preserve osmo variant",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260110-osmo",
			expectedProfile:    "nke-1.31.9-260110-osmo",
			expectError:        false,
		},
		{
			name:               "current profile not among the releases - returns error (no guessing)",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-unknown-profile",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "not found",
		},
		{
			name:               "no matching profile found",
			k8sVersion:         "1.32.0",
			currentProfileName: "",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "no OSProfile found for Kubernetes version 1.32.0",
		},
		{
			name:               "no matching arm64 profile for version",
			k8sVersion:         "1.30.5",
			currentProfileName: "nke-1.31.9-260115-arm64",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "no OSProfile found for Kubernetes version 1.30.5",
		},
		{
			name:               "pick newest by build_date when multiple matches",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260101",
			expectedProfile:    "nke-1.31.9-260120",
			expectError:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReleaseReconciler(t, releases)
			result, err := r.selectUpgradeRelease(t.Context(), true, tt.k8sVersion, tt.currentProfileName)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedProfile, result)
			}
		})
	}
}

func TestSelectUpgradeRelease_ListError(t *testing.T) {
	r := testFailingReleaseReconciler(t, fmt.Errorf("releases unavailable"))

	_, err := r.selectUpgradeRelease(t.Context(), true, "1.31.9", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing ClusterReleases")
}

func TestSelectUpgradeRelease_BuildDateOrdering(t *testing.T) {
	r := testReleaseReconciler(t, releaseFixtures{
		newReleaseFixture("nke-1.30.5-260101", "1.30.5", "amd64", "", "2026-01-01", false, false),
		newReleaseFixture("nke-1.31.9-260105", "1.31.9", "amd64", "", "2026-01-05", false, false),
		newReleaseFixture("nke-1.31.9-260120", "1.31.9", "amd64", "", "2026-01-20", false, false),
		newReleaseFixture("nke-1.31.9-260115", "1.31.9", "amd64", "", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260110", "1.31.9", "amd64", "", "2026-01-10", false, false),
		newReleaseFixture("nke-1.31.9-260125", "1.31.9", "amd64", "", "2026-01-25", false, false),
	})

	result, err := r.selectUpgradeRelease(t.Context(), true, "1.31.9", "nke-1.30.5-260101")
	require.NoError(t, err)
	assert.Equal(t, "nke-1.31.9-260125", result, "Should pick profile with newest build_date (2026-01-25)")
}

func TestSelectUpgradeRelease_VariantPreservation(t *testing.T) {
	r := testReleaseReconciler(t, releaseFixtures{
		newReleaseFixture("nke-1.30.5-260101-ovn", "1.30.5", "amd64", "ovn", "2026-01-01", false, false),
		newReleaseFixture("nke-1.31.9-260115", "1.31.9", "amd64", "", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260115-ovn", "1.31.9", "amd64", "ovn", "2026-01-15", false, false),
		newReleaseFixture("nke-1.31.9-260120-ovn", "1.31.9", "amd64", "ovn", "2026-01-20", false, false),
	})

	result, err := r.selectUpgradeRelease(t.Context(), true, "1.31.9", "nke-1.30.5-260101-ovn")
	require.NoError(t, err)
	assert.Equal(t, "nke-1.31.9-260120-ovn", result, "Should preserve 'ovn' variant and pick newest")
}

func TestSelectUpgradeRelease_SkipsDeprecatedAndKeepsInjected(t *testing.T) {
	r := testReleaseReconciler(t, releaseFixtures{
		newReleaseFixture("nke-1.30.5-260101", "1.30.5", "amd64", "", "2026-01-01", false, true),
		newReleaseFixture("nke-1.31.9-retired", "1.31.9", "amd64", "", "2026-02-01", false, true),
		newReleaseFixture("nke-1.31.9-fixed", "1.31.9", "amd64", "", "2026-01-10", false, false),
		newReleaseFixture("nke-injected", "", "amd64", "", "2026-01-20", true, false),
	})

	result, err := r.selectUpgradeRelease(t.Context(), true, "1.35.0", "nke-1.30.5-260101")
	require.NoError(t, err)
	assert.Equal(t, "nke-injected", result)
}

func TestSelectUpgradeRelease_LoggingBehavior(t *testing.T) {
	r := testReleaseReconciler(t, releaseFixtures{
		newReleaseFixture("nke-1.30.5-260101-ovn", "1.30.5", "amd64", "ovn", "2026-01-01", false, false),
		newReleaseFixture("nke-1.31.9-260115-ovn", "1.31.9", "amd64", "ovn", "2026-01-15", false, false),
	})
	r.logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	result, err := r.selectUpgradeRelease(t.Context(), true, "1.31.9", "nke-1.30.5-260101-ovn")
	require.NoError(t, err)
	assert.Equal(t, "nke-1.31.9-260115-ovn", result)
}

// releaseFixture is a ClusterRelease and the MachineImage it boots.
type releaseFixture struct {
	release *enginev1.ClusterRelease
	image   *enginev1.MachineImage
}

type releaseFixtures []releaseFixture

func (f releaseFixtures) objects() []ctrlclient.Object {
	objects := make([]ctrlclient.Object, 0, 2*len(f))
	for _, fixture := range f {
		objects = append(objects, fixture.release, fixture.image)
	}
	return objects
}

func testReleaseReconciler(t *testing.T, fixtures releaseFixtures) *reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, enginev1.AddToScheme(scheme))
	return &reconciler{
		logger:    slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})),
		k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(fixtures.objects()...).Build(),
	}
}

// testFailingReleaseReconciler reads releases from an API server that is down.
func testFailingReleaseReconciler(t *testing.T, err error) *reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, enginev1.AddToScheme(scheme))
	return &reconciler{
		logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})),
		k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
				return err
			},
		}).Build(),
	}
}

// newReleaseFixture builds a ClusterRelease named after, and booting, an image
// with the given attributes. An injected release carries no Kubernetes version.
func newReleaseFixture(name, version, architecture, variant, buildDate string, injected, deprecated bool) releaseFixture {
	injectedValue := "false"
	if injected {
		injectedValue = "true"
		version = ""
	}
	return releaseFixture{
		release: &enginev1.ClusterRelease{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Annotations: map[string]string{
					enginev1.MachineInitKubernetesVersionAnnotation:         version,
					enginev1.MachineInitKubernetesVersionInjectedAnnotation: injectedValue,
				},
			},
			Spec: enginev1.ClusterReleaseSpec{
				MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(name)},
				MachineInit:     enginev1.MachineInit{UserDataTemplate: "#cloud-config\n"},
				Lifecycle:       enginev1.ReleaseLifecycle{Deprecated: deprecated},
			},
		},
		image: &enginev1.MachineImage{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: enginev1.MachineImageSpec{
				OperatingSystem: enginev1.OperatingSystem{Family: "ubuntu", Version: "22.04"},
				Architecture:    architecture,
				Variant:         variant,
				BuildDate:       buildDate,
				NICo:            enginev1.NICoBoot{IPXEScript: "#!ipxe\n"},
			},
		},
	}
}
