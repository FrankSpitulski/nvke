// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package machinerelease_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	enginev1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/machinerelease"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestResolveClusterReleaseIncludesDeprecatedAndReturnsImage(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("ubuntu-24", "amd64", "fde", "24.04", "2026-03-23", "#!ipxe\necho boot\n"),
		clusterRelease("nke-old", "ubuntu-24", true),
	)

	release, img, err := machinerelease.ResolveClusterRelease(t.Context(), reader, "nke-old")
	require.NoError(t, err)
	assert.True(t, release.Spec.Lifecycle.Deprecated)
	assert.Equal(t, "#cloud-config\n", release.Spec.MachineInit.UserDataTemplate)
	assert.Equal(t, "#!ipxe\necho boot\n", img.Spec.NICo.IPXEScript)
	assert.Equal(t, "amd64", img.Spec.Architecture)
	assert.Equal(t, "fde", img.Spec.Variant)
	assert.Equal(t, "24.04", img.Spec.OperatingSystem.Version)
	assert.Equal(t, "2026-03-23", img.Spec.BuildDate)

	version, injected, err := machinerelease.ParseKubernetesVersionAnnotations(release)
	require.NoError(t, err)
	assert.Equal(t, "1.31.12", version)
	assert.False(t, injected)
}

func TestResolveNodePoolReleaseInjected(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("ubuntu-24", "arm64", "", "24.04", "2026-09-21", "#!ipxe\n"),
		nodePoolRelease("nke-injected", "ubuntu-24", "", true, false, "#cloud-config\nhostname: {{ .Hostname }}\n"),
	)

	release, img, err := machinerelease.ResolveNodePoolRelease(t.Context(), reader, "nke-injected")
	require.NoError(t, err)
	assert.False(t, release.Spec.Lifecycle.Deprecated)
	assert.Equal(t, "arm64", img.Spec.Architecture)

	version, injected, err := machinerelease.ParseKubernetesVersionAnnotations(release)
	require.NoError(t, err)
	assert.Empty(t, version)
	assert.True(t, injected)
}

func TestResolveHasNoFallbackBetweenKinds(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		clusterRelease("only-cluster", "img", false),
	)

	_, _, err := machinerelease.ResolveNodePoolRelease(t.Context(), reader, "only-cluster")
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
	assert.Contains(t, err.Error(), `NodePoolRelease "only-cluster" is not installed`)
}

func TestResolveMissingRelease(t *testing.T) {
	t.Parallel()

	_, _, err := machinerelease.ResolveNodePoolRelease(t.Context(), fakeClient(t), "missing")
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
	assert.Contains(t, err.Error(), `NodePoolRelease "missing" is not installed`)
}

func TestResolveEmptyName(t *testing.T) {
	t.Parallel()

	_, _, err := machinerelease.ResolveClusterRelease(t.Context(), fakeClient(t), "")
	require.ErrorContains(t, err, "release name is empty")
}

func TestResolveMissingMachineImage(t *testing.T) {
	t.Parallel()

	_, _, err := machinerelease.ResolveClusterRelease(t.Context(), fakeClient(t, clusterRelease("nke-old", "gone", false)), "nke-old")
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
	assert.Contains(t, err.Error(), `MachineImage "gone" is not installed`)
}

func TestParseKubernetesVersionAnnotationsRejectsBadInjectionAnnotation(t *testing.T) {
	t.Parallel()

	release := clusterRelease("nke-old", "ubuntu-24", false)
	release.Annotations[enginev1.MachineInitKubernetesVersionInjectedAnnotation] = "maybe"

	_, _, err := machinerelease.ParseKubernetesVersionAnnotations(release)
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
	assert.Contains(t, err.Error(), "maybe")
}

func TestCheckCRDsPassesWhenKindsAreRegistered(t *testing.T) {
	t.Parallel()

	require.NoError(t, machinerelease.CheckCRDs(t.Context(), fakeClient(t)))
}

func TestCheckCRDsFailsWhenKindsAreNotInScheme(t *testing.T) {
	t.Parallel()

	reader := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	err := machinerelease.CheckCRDs(t.Context(), reader)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checking MachineImage")
}

// An API server that serves the group without the kind is what a cluster the
// release CRDs were never applied to looks like, and it is the case the
// startup check exists for.
func TestCheckCRDsNamesTheKindThatIsNotInstalled(t *testing.T) {
	t.Parallel()

	notInstalled := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: enginev1.GroupVersion.Group, Kind: "MachineImage"}}

	err := machinerelease.CheckCRDs(t.Context(), failingClient(t, notInstalled))
	require.ErrorContains(t, err, "MachineImage CRD is not installed")
}

func TestSelectUpgradeTargetSkipsDeprecatedAndPrefersNewestBuild(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img-old", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		image("img-new", "amd64", "", "22.04", "2026-01-20", "#!ipxe\n"),
		image("img-retired", "amd64", "", "22.04", "2026-02-01", "#!ipxe\n"),
		image("img-arm", "arm64", "", "22.04", "2026-03-01", "#!ipxe\n"),
		nodePoolRelease("old", "img-old", "1.31.9", false, false, "#cloud-config\n"),
		nodePoolRelease("newer", "img-new", "1.31.9", false, false, "#cloud-config\n"),
		nodePoolRelease("retired-newest", "img-retired", "1.31.9", false, true, "#cloud-config\n"),
		nodePoolRelease("other-arch", "img-arm", "1.31.9", false, false, "#cloud-config\n"),
		nodePoolRelease("other-version", "img-new", "1.30.5", false, false, "#cloud-config\n"),
		nodePoolRelease("injected", "img-old", "", true, false, "#cloud-config\n"),
	)

	name, img, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, false, "1.31.9", image("current", "amd64", "", "22.04", "", ""))
	require.NoError(t, err)
	assert.Equal(t, "newer", name)
	assert.Equal(t, "2026-01-20", img.Spec.BuildDate)
}

func TestSelectUpgradeTargetKeepsTheCurrentOSFamily(t *testing.T) {
	t.Parallel()

	otherFamily := image("img-debian", "amd64", "", "22.04", "2026-02-01", "#!ipxe\n")
	otherFamily.Spec.OperatingSystem.Family = "debian"

	reader := fakeClient(t,
		image("img-ubuntu", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		otherFamily,
		nodePoolRelease("same-family", "img-ubuntu", "1.31.9", false, false, "#cloud-config\n"),
		nodePoolRelease("other-family-newer", "img-debian", "1.31.9", false, false, "#cloud-config\n"),
	)

	name, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, false, "1.31.9", image("current", "amd64", "", "22.04", "", ""))
	require.NoError(t, err)
	assert.Equal(t, "same-family", name, "the other family's image is newer, but an upgrade must not change distribution")
}

func TestSelectUpgradeTargetAcceptsInjectedRelease(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "fde", "24.04", "2026-09-21", "#!ipxe\n"),
		clusterRelease("injected", "img", false, injectedAnnotations()),
	)

	name, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, true, "1.35.0", image("current", "amd64", "fde", "24.04", "", ""))
	require.NoError(t, err)
	assert.Equal(t, "injected", name)
}

func TestSelectUpgradeTargetSearchesOnlyTheRolesKind(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		nodePoolRelease("worker-only", "img", "1.31.9", false, false, "#cloud-config\n"),
	)

	_, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, true, "1.31.9", image("current", "amd64", "", "22.04", "", ""))
	require.ErrorContains(t, err, "no OSProfile found for Kubernetes version 1.31.9")
}

func TestSelectUpgradeTargetSkipsReleasesThatCannotBeResolved(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		nodePoolRelease("good", "img", "1.31.12", false, false, "#cloud-config\n"),
		nodePoolRelease("no-image", "gone", "1.31.12", false, false, "#cloud-config\n"),
	)

	name, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, false, "1.31.12", image("current", "amd64", "", "22.04", "", ""))
	require.NoError(t, err)
	assert.Equal(t, "good", name)

	_, _, err = machinerelease.ResolveNodePoolRelease(t.Context(), reader, "no-image")
	require.ErrorIs(t, err, machinerelease.ErrInvalidRelease)
}

func TestSelectUpgradeTargetWithoutCurrentMatchesOnlyBareImages(t *testing.T) {
	t.Parallel()

	reader := fakeClient(t,
		image("img", "amd64", "", "22.04", "2026-01-01", "#!ipxe\n"),
		clusterRelease("cp", "img", false),
	)

	_, _, err := machinerelease.SelectUpgradeTarget(t.Context(), reader, true, "1.31.12", nil)
	require.ErrorContains(t, err, "no OSProfile found")
}

func fakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, enginev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func image(name, arch, variant, ubuntu, buildDate, ipxe string) *enginev1.MachineImage {
	return &enginev1.MachineImage{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: enginev1.MachineImageSpec{
			OperatingSystem: enginev1.OperatingSystem{Family: "ubuntu", Version: ubuntu},
			Architecture:    arch,
			Variant:         variant,
			BuildDate:       buildDate,
			NICo:            enginev1.NICoBoot{IPXEScript: ipxe},
		},
	}
}

// clusterRelease builds a ClusterRelease that boots Kubernetes 1.31.12, or
// injects the version when annotations are given.
func clusterRelease(name, imageName string, deprecated bool, annotations ...map[string]string) *enginev1.ClusterRelease {
	release := &enginev1.ClusterRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: versionAnnotations("1.31.12", false),
		},
		Spec: enginev1.ClusterReleaseSpec{
			MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(imageName)},
			MachineInit:     enginev1.MachineInit{UserDataTemplate: "#cloud-config\n"},
			Lifecycle:       enginev1.ReleaseLifecycle{Deprecated: deprecated},
		},
	}
	if len(annotations) > 0 {
		release.Annotations = annotations[0]
	}
	return release
}

func nodePoolRelease(name, imageName, version string, injected, deprecated bool, userData string) *enginev1.NodePoolRelease {
	return &enginev1.NodePoolRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: versionAnnotations(version, injected),
		},
		Spec: enginev1.NodePoolReleaseSpec{
			MachineImageRef: enginev1.MachineImageReference{Name: enginev1.ResourceName(imageName)},
			MachineInit:     enginev1.MachineInit{UserDataTemplate: userData},
			Lifecycle:       enginev1.ReleaseLifecycle{Deprecated: deprecated},
		},
	}
}

func injectedAnnotations() map[string]string {
	return versionAnnotations("", true)
}

func versionAnnotations(version string, injected bool) map[string]string {
	injectedValue := "false"
	if injected {
		injectedValue = "true"
		version = ""
	}
	return map[string]string{
		enginev1.MachineInitKubernetesVersionAnnotation:         version,
		enginev1.MachineInitKubernetesVersionInjectedAnnotation: injectedValue,
	}
}

const referencedImageName = "test-image"

// referencedClient is a cluster holding the given objects plus the image every
// test release boots.
func referencedClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	all := append([]client.Object{image(referencedImageName, "amd64", "", "22.04", "2026-01-01", "#!ipxe\n")}, objects...)
	return fakeClient(t, all...)
}

// failingClient fails every read, like an unreachable API server.
func failingClient(t *testing.T, err error) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, enginev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return err
		},
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return err
		},
	}).Build()
}

// mockReferenced is a database whose live resources reference the given names.
func mockReferenced(referenced db.ReferencedReleases) *testutil.MockDB {
	database := &testutil.MockDB{}
	database.On("ListReferencedReleaseNames", mock.Anything).Return(referenced, nil)
	return database
}

func testClusterRelease(name string, deprecated bool) *enginev1.ClusterRelease {
	return clusterRelease(name, referencedImageName, deprecated)
}

func testNodePoolRelease(name string, deprecated bool) *enginev1.NodePoolRelease {
	return nodePoolRelease(name, referencedImageName, "1.31.12", false, deprecated, "#cloud-config\n")
}

func TestCheckReferencedReleases(t *testing.T) {
	installed := []client.Object{
		testNodePoolRelease("nke-worker", false),
		testClusterRelease("nke-cp", false),
		testNodePoolRelease("nke-old", true),
		testClusterRelease("nke-old-cp", true),
	}
	noImage := testClusterRelease("nke-no-image", false)
	noImage.Spec.MachineImageRef.Name = "gone"
	badAnnotation := testNodePoolRelease("nke-bad-annotation", false)
	badAnnotation.Annotations = map[string]string{}

	tests := []struct {
		name       string
		referenced db.ReferencedReleases
		objects    []client.Object
		failWith   error
		wantErrs   []string
	}{
		{name: "nothing is referenced"},
		{
			name:       "every selection is installed",
			referenced: db.ReferencedReleases{ControlPlane: []string{"nke-cp"}, Worker: []string{"nke-worker"}},
			objects:    installed,
		},
		{
			name:       "deprecated releases still count as installed",
			referenced: db.ReferencedReleases{ControlPlane: []string{"nke-old-cp"}, Worker: []string{"nke-old"}},
			objects:    installed,
		},
		{
			name:       "a control-plane selection names a missing ClusterRelease",
			referenced: db.ReferencedReleases{ControlPlane: []string{"nke-missing"}},
			objects:    installed,
			wantErrs:   []string{`1 referenced release(s) are unavailable`, `ClusterRelease "nke-missing" is not installed`},
		},
		{
			name:       "a control-plane selection needs a ClusterRelease, not a NodePoolRelease",
			referenced: db.ReferencedReleases{ControlPlane: []string{"nke-worker"}},
			objects:    installed,
			wantErrs:   []string{`ClusterRelease "nke-worker" is not installed`},
		},
		{
			name:       "a worker selection needs a NodePoolRelease, not a ClusterRelease",
			referenced: db.ReferencedReleases{Worker: []string{"nke-cp"}},
			objects:    installed,
			wantErrs:   []string{`NodePoolRelease "nke-cp" is not installed`},
		},
		{
			name:       "every missing release is reported",
			referenced: db.ReferencedReleases{ControlPlane: []string{"nke-a"}, Worker: []string{"nke-b", "nke-c"}},
			objects:    installed,
			wantErrs: []string{
				`3 referenced release(s) are unavailable`,
				`ClusterRelease "nke-a" is not installed`,
				`NodePoolRelease "nke-b" is not installed`,
				`NodePoolRelease "nke-c" is not installed`,
			},
		},
		{
			name:       "a release whose MachineImage is not installed is missing",
			referenced: db.ReferencedReleases{ControlPlane: []string{"nke-no-image"}},
			objects:    append([]client.Object{noImage}, installed...),
			wantErrs:   []string{`release "nke-no-image"`, `MachineImage "gone" is not installed`},
		},
		{
			name:       "a release with bad annotations is missing",
			referenced: db.ReferencedReleases{Worker: []string{"nke-bad-annotation"}},
			objects:    append([]client.Object{badAnnotation}, installed...),
			wantErrs:   []string{`release "nke-bad-annotation" is missing annotation`},
		},
		{
			name:       "a read failure is not reported as a missing release",
			referenced: db.ReferencedReleases{Worker: []string{"nke-worker"}},
			failWith:   errors.New("connection refused"),
			wantErrs:   []string{"connection refused"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reader client.Reader = referencedClient(t, tt.objects...)
			if tt.failWith != nil {
				reader = failingClient(t, tt.failWith)
			}

			err := machinerelease.CheckReferencedReleases(t.Context(), mockReferenced(tt.referenced), reader)

			if len(tt.wantErrs) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tt.wantErrs {
				require.ErrorContains(t, err, want)
			}
			if tt.failWith != nil {
				assert.NotErrorIs(t, err, machinerelease.ErrInvalidRelease)
			}
		})
	}
}

func TestCheckReferencedReleasesReadsNothingWhenNothingIsReferenced(t *testing.T) {
	reader := failingClient(t, errors.New("must not be called"))

	require.NoError(t, machinerelease.CheckReferencedReleases(t.Context(), mockReferenced(db.ReferencedReleases{}), reader))
}

func TestCheckReferencedReleasesRejectsBrokenInputs(t *testing.T) {
	t.Run("missing reader", func(t *testing.T) {
		require.ErrorContains(t, machinerelease.CheckReferencedReleases(t.Context(), &testutil.MockDB{}, nil), "release reader is not configured")
	})
	t.Run("missing database", func(t *testing.T) {
		require.ErrorContains(t, machinerelease.CheckReferencedReleases(t.Context(), nil, fakeClient(t)), "database is not configured")
	})
	t.Run("database error", func(t *testing.T) {
		database := &testutil.MockDB{}
		database.On("ListReferencedReleaseNames", mock.Anything).Return(db.ReferencedReleases{}, errors.New("db down"))
		require.ErrorContains(t, machinerelease.CheckReferencedReleases(t.Context(), database, fakeClient(t)), "db down")
	})
}
