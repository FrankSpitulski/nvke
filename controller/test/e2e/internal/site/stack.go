// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/pkg/deploypath"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/artifact"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
)

const (
	// defaultStorageClassManifest is rancher local-path, what the reference
	// site used because its instance type has one NVMe with root on it.
	defaultStorageClassManifest = "https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.31/deploy/local-path-storage.yaml"
	defaultStorageClassName     = "local-path"

	// oidcPort is the OIDC discovery provider's NodePort.
	oidcPort = 30443

	nodeAgentPort         = "15005"
	spireServerAPIAddress = "spire-server.spire-server.svc.cluster.local:443"

	defaultNicoAPIName = "forge"

	// valuesCleanupTimeout bounds removing the rendered values files. Short on
	// purpose: it is one rm over a session that was answering a moment ago.
	valuesCleanupTimeout = 2 * time.Minute
)

// Where the deploy path lives on the node. assetsArchive is transient: it is
// uploaded, untarred into assetsDir, and deleted in the same command.
const (
	assetsArchive = workDir + "/assets.tar.gz"
	assetsDir     = workDir + "/assets"
)

// The deploy path's releases. Each gets a values file named after it, passed
// with --values-for <release>=<file>.
const (
	releaseController = "controller"
	releaseSpire      = "spire"
	releasePostgreSQL = "postgresql"
	releaseTemporal   = "temporal"
)

func valuesFile(release string) string { return release + ".yaml" }

// tpmServerChecksum is the SHA-256 of the TPM attestor plugin binary the
// SPIRE server loads, per node architecture.
func tpmServerChecksum(arch string) (string, bool) {
	switch arch {
	case "x86_64":
		return "46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc", true
	case "arm64":
		return "b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16", true
	default:
		return "", false
	}
}

// envVar is an entry in a container's env list.
type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Image describes container content. A digest takes precedence over a tag;
// authentication and archive integrity belong to the caller staging the input.
type Image struct {
	Repository string
	Tag        string
	Digest     string
}

func (i Image) valid() bool { return i.Repository != "" && (i.Tag != "" || i.Digest != "") }

// Ref is the image as repository@digest, or repository:tag when no digest is supplied.
func (i Image) Ref() string {
	if i.Digest != "" {
		return i.Repository + "@" + i.Digest
	}
	return i.Repository + ":" + i.Tag
}

func (i Image) values() map[string]any {
	values := map[string]any{"repository": i.Repository, "tag": i.Tag}
	if i.Digest != "" {
		values["digest"] = i.Digest
	}
	return values
}

// CAPIProviderConfig is how the Cluster API infrastructure provider the stack
// installs reaches the infrastructure it creates tenant machines on. That
// provider needs an endpoint and a credential of its own, because the site
// controller creates machines long after this run is over.
//
// Named for CAPI because three things here are called a provider: infra.Provider
// creates the site node from the test process, environment.Environment.Provider
// names the dialect a file is written in, and this one is the CAPI provider
// running in the cluster the run just built.
//
// An interface because which provider is installed follows which one built the
// site, and nothing between here and the deploy should have to name it. The
// value renders into bootstrap-config.json under its own key, and the deploy
// tree's credentials template reaches for that key, so adding a provider is a
// type here and a template there with nothing in between.
type CAPIProviderConfig interface {
	// ConfigKey is the key this renders under in bootstrap-config.json.
	ConfigKey() string

	// Validate reports what the provider cannot be configured without. Called
	// before the deploy stages anything, so a missing credential fails the
	// run rather than the controller that reads the Secret.
	Validate() error
}

// Nico is where the site's CAPNICo reaches the NICo API.
type Nico struct {
	Endpoint string `json:"Endpoint"`
	OrgID    string `json:"OrgID"`
	Token    string `json:"Token"`
	APIName  string `json:"APIName"`
}

// NewNico is the CAPNICo configuration, with the API name defaulted. The name
// is the path segment the API is served under and is the same for every site,
// so a caller states the three values that differ.
func NewNico(endpoint, orgID, token string) Nico {
	return Nico{Endpoint: endpoint, OrgID: orgID, Token: token, APIName: defaultNicoAPIName}
}

// ConfigKey implements CAPIProviderConfig. "Nico" is what
// 20-nico-credentials.yaml.tmpl reaches for.
func (n Nico) ConfigKey() string { return "Nico" }

// Validate implements CAPIProviderConfig.
func (n Nico) Validate() error {
	if n.Endpoint == "" || n.OrgID == "" || n.Token == "" {
		return errors.New("NICo endpoint, org and token are required")
	}
	return nil
}

// Registry is the credential the site pulls private images with.
type Registry struct {
	Server   string `json:"Server"`
	Username string `json:"Username"`
	Token    string `json:"Token"`
}

// Tenant is the credential CWE creates tenant clusters' admin with.
type Tenant struct {
	User     string `json:"User"`
	Password string `json:"Password"`
}

// Stack deploys the NVKE stack with a deploy path from the Stack: it does what
// the deploy path's README says a driver does around the install, and renders
// the values only a site knows.
//
// The deploy path and the scripts that apply it come from the staged assets,
// not from this suite, so the suite follows whatever deploy path the checkout
// it stages carries.
type Stack struct {
	// Assets are the deploy path and the scripts that apply it, staged to
	// the node with their layout intact, which the deploy path depends on: a
	// release reaches its chart by relative path. ExportDeployAssets builds
	// them.
	//
	// Nothing in the bootstrap reads them: nvkectl pulls its charts from their
	// repositories and embeds its values. These are the deploy's own
	// installer, tree and scripts.
	Assets DeployAssets

	Controller Image
	Profiles   Image

	// ChartOverrides are packaged chart files on the runner, keyed by deploy item.
	// These files are staged verbatim. Only the private exported release
	// metadata is projected to its staged archive; the executor is unchanged.
	ChartOverrides map[string]string

	// Provider is how the installed Cluster API provider reaches its
	// infrastructure. Required: a stack with no way to create machines
	// deploys and then cannot do its job.
	Provider CAPIProviderConfig
	Registry Registry
	Tenant   Tenant

	// Arch is the node's architecture, as uname -m reports it. It selects the
	// TPM attestor checksum.
	Arch string

	// DatabasePassword is set on PostgreSQL and on both Temporal datastores.
	// Empty generates one per run.
	DatabasePassword string

	// StorageClassManifest installs the site's default StorageClass, named
	// StorageClassName. They default to rancher local-path; a site with real
	// storage names its own.
	StorageClassManifest string
	StorageClassName     string

	// ControllerEnv is appended to the controller's environment.
	ControllerEnv map[string]string
}

// DeployAssets are a ref's deploy path and the scripts that apply it,
// exported onto the runner.
type DeployAssets struct {
	// Dir is the export. Layout.Export, relative to it, is what is staged.
	Dir string
	// Layout is where Dir keeps the executor and the deploy path, which
	// depends on the ref it came from. Zero is CurrentLayout().
	Layout DeployLayout
}

// ExportDeployAssets exports ref's deploy assets from repo and checks that
// the controller name they install is the one the suite looks for. cleanup
// removes the export; the caller owns when it runs.
//
// Call it before any machine is taken, not at deploy time, so a release that
// predates the supported layout or disagrees about the name fails here on
// the runner rather than after a node has been provisioned.
func ExportDeployAssets(ctx context.Context, repo, ref string) (DeployAssets, func(), error) {
	layout, err := ResolveLayout(ctx, repo, ref)
	if err != nil {
		return DeployAssets{}, nil, err
	}
	dir, err := ExportRef(ctx, repo, ref, layout.Export)
	if err != nil {
		return DeployAssets{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := CheckControllerName(dir, layout.DeployPath); err != nil {
		cleanup()
		return DeployAssets{}, nil, err
	}
	return DeployAssets{Dir: dir, Layout: layout}, cleanup, nil
}

// stageAssets copies the deploy path onto the node, with the layout the
// scripts in it expect.
func (d Stack) stageAssets(ctx context.Context, s infra.Session) error {
	archive, err := archiveAssets(d.Assets.Dir, d.Assets.Layout.Export)
	if err != nil {
		return err
	}
	if err := s.Put(ctx, assetsArchive, archive, 0o600); err != nil {
		return err
	}
	cmd := fmt.Sprintf("rm -rf %[1]s && mkdir -p %[1]s && tar -C %[1]s -xzf %[2]s && rm -f %[2]s",
		shellQuote(assetsDir), shellQuote(assetsArchive))
	if out, err := s.Run(ctx, asRoot(cmd)); err != nil {
		return fmt.Errorf("stage the deploy path: %w\n%s", err, tail(out))
	}
	return nil
}

// sortedKeys renders a map in a stable order, so the same input produces the
// same command twice.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Deploy implements Deployer.
func (d Stack) Deploy(ctx context.Context, s infra.Session, t DeployTarget) error {
	d = d.withDefaults()
	if err := d.validate(); err != nil {
		return err
	}
	releases, releaseErr := chartOverrideReleases(d.Assets, d.ChartOverrides)
	if releaseErr != nil {
		return releaseErr
	}
	if len(d.ChartOverrides) != 0 {
		if err := admitChartExecutor(ctx, d.Assets, sortedKeys(d.ChartOverrides)[0]); err != nil {
			return err
		}
	}
	if d.DatabasePassword == "" {
		password, err := randomPassword()
		if err != nil {
			return err
		}
		d.DatabasePassword = password
	}

	if err := d.stageAssets(ctx, s); err != nil {
		return err
	}
	valuesDir := path.Join(workDir, "values")
	// Register cleanup before any chart or values Put, including partial
	// staging failures. Detached and bounded so cancellation still removes
	// the files carrying bootstrap tokens and database credentials.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), valuesCleanupTimeout)
		defer cancel()
		_, _ = s.Run(cctx, asRoot("rm -rf "+shellQuote(valuesDir)+" "+shellQuote(path.Join(workDir, "charts"))))
	}()
	for _, item := range sortedKeys(d.ChartOverrides) {
		data, err := os.ReadFile(d.ChartOverrides[item])
		if err != nil {
			return fmt.Errorf("read staged chart %s: %w", item, err)
		}
		slog.InfoContext(ctx, "staging packaged chart", "item", item, "path", d.ChartOverrides[item], "sha256", fmt.Sprintf("%x", sha256.Sum256(data)))
		if err := s.Put(ctx, stagedChart(item), data, 0o600); err != nil {
			return fmt.Errorf("stage chart %s: %w", item, err)
		}
		metadata := path.Join(assetsDir, d.Assets.Layout.DeployPath, item, "release.yaml")
		if err := s.Put(ctx, metadata, releases[item], 0o600); err != nil {
			return fmt.Errorf("stage release metadata %s: %w", item, err)
		}
	}

	overlay, err := s.Fetch(ctx, path.Join(assetsDir, d.Assets.Layout.DeployPath, releaseSpire, "overlay.yaml"))
	if err != nil {
		return fmt.Errorf("read the deploy path's SPIRE overlay: %w", err)
	}
	files, err := d.render(t, overlay)
	if err != nil {
		return err
	}

	for name, data := range files {
		if err := s.Put(ctx, path.Join(valuesDir, name), data, 0o600); err != nil {
			return err
		}
	}

	for _, step := range d.commands(t, valuesDir) {
		if out, err := s.Run(ctx, asRoot(step.cmd)); err != nil {
			return fmt.Errorf("%s: %w\n%s", step.name, err, tail(out))
		}
	}
	return nil
}

func (d Stack) withDefaults() Stack {
	if d.Assets.Layout.Executor == "" {
		d.Assets.Layout = CurrentLayout()
	}
	if d.StorageClassManifest == "" {
		d.StorageClassManifest = defaultStorageClassManifest
		if d.StorageClassName == "" {
			d.StorageClassName = defaultStorageClassName
		}
	}
	return d
}

// CheckControllerName fails when the deploy path installs the controller
// under a name the suite is not configured to look for.
//
// These are two owners of one string: the deploy path, exported from the
// controller version under test, and NKE_CONTROLLER_NAMESPACE and
// NKE_CONTROLLER_SERVICE, which default to nke-controller. Nothing makes the
// two move together.
//
// Getting it wrong is expensive rather than obvious: the readiness probe
// opens a port-forward onto a Service that does not exist, the forwarder
// retries the NotFound rather than reporting it, and the run spends ten
// minutes reporting that the controller will not come up while it is healthy
// in the namespace beside it. So this runs on the runner, before any machine
// is taken.
func CheckControllerName(assets, deployPath string) error {
	deployed, err := controllerNameFromDeployPath(assets, deployPath)
	if err != nil {
		return err
	}
	if deployed == controller.Namespace() && deployed == controller.Service() {
		return nil
	}
	return fmt.Errorf(
		"the deploy path installs the controller as %q but the suite looks for %s/%s: set %s and %s to %q, or qualify a controller whose deploy path matches",
		deployed, controller.Namespace(), controller.Service(), controller.EnvNamespace, controller.EnvService, deployed,
	)
}

// controllerNameFromDeployPath is the name a set of exported assets installs
// CWE under, read from the HelmRelease rather than assumed.
func controllerNameFromDeployPath(assets, deployPath string) (string, error) {
	releasePath := ControllerReleasePath(deployPath)
	raw, err := os.ReadFile(filepath.Join(assets, releasePath))
	if err != nil {
		return "", fmt.Errorf("reading the deploy path's controller release: %w", err)
	}

	var release struct {
		Release struct {
			Namespace string `json:"namespace"`
		} `json:"release"`
	}
	if err := yaml.Unmarshal(raw, &release); err != nil {
		return "", fmt.Errorf("parsing %s: %w", releasePath, err)
	}
	if release.Release.Namespace == "" {
		return "", fmt.Errorf("%s names no release namespace, so the name CWE is installed under cannot be read", releasePath)
	}
	return release.Release.Namespace, nil
}

// controllerName is the namespace, Service and ServiceAccount the controller
// is installed under, and so the last segment of its SPIFFE ID.
//
// Read from the controller package rather than written out here, because this
// file does not get to choose it. The deploy path does, and
// CheckControllerName fails the run before any spec when the two disagree.
func controllerName() string { return controller.Service() }

// spireAdminPath is CWE's SPIFFE ID below the trust domain. A name that does
// not match what the deploy path installed leaves CWE denied every
// authorization check, which reads as an RBAC fault rather than a naming one.
func spireAdminPath() string { return "/nke-service/" + controllerName() }

func (d Stack) validate() error {
	var problems []string
	for _, image := range []struct {
		name  string
		image Image
	}{{"controller", d.Controller}, {"profiles", d.Profiles}} {
		if err := artifact.ValidateDigest(image.image.Digest); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", image.name, err))
		}
		if err := artifact.ValidateRepository(image.image.Repository, image.image.Digest); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", image.name, err))
		}
	}
	if !d.Controller.valid() {
		problems = append(problems, "controller image needs a repository and a tag or digest")
	}
	if !d.Profiles.valid() {
		problems = append(problems, "profiles image needs a repository and a tag or digest")
	}
	if d.Assets.Dir == "" {
		problems = append(problems, "no assets")
	} else if _, err := os.Stat(d.Assets.Dir); err != nil {
		problems = append(problems, fmt.Sprintf("assets: %v", err))
	}
	for _, rel := range d.Assets.Layout.Export {
		if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
			problems = append(problems, fmt.Sprintf("asset path %q is not inside the checkout", rel))
		}
	}
	if err := ValidateChartOverrides(d.ChartOverrides); err != nil {
		problems = append(problems, err.Error())
	}
	if _, ok := tpmServerChecksum(d.Arch); !ok {
		problems = append(problems, fmt.Sprintf("no TPM attestor checksum for architecture %q", d.Arch))
	}
	if d.StorageClassName == "" {
		problems = append(problems, "a storage class manifest needs the name of the class it creates")
	}
	if d.Provider == nil {
		problems = append(problems, "no provider configuration, so the stack could not create a machine")
	} else if err := d.Provider.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New("stack deployer: " + strings.Join(problems, "; "))
	}
	return nil
}

type step struct {
	name string
	cmd  string
}

// ValidateChartOverrides checks resolved runner paths before provisioning or
// deployment. Chart parsing and artifact authentication remain caller-owned.
func ValidateChartOverrides(charts map[string]string) error {
	var problems []string
	for _, item := range sortedKeys(charts) {
		info, err := os.Stat(charts[item])
		if err != nil {
			problems = append(problems, fmt.Sprintf("chart %s: %v", item, err))
		} else if !info.Mode().IsRegular() || info.Size() == 0 {
			problems = append(problems, fmt.Sprintf("chart %s must be a nonempty regular file", item))
		}
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// ValidateChartOverridesForAssets checks chart roles and release metadata
// against the exported deployment layout before provisioning a site.
func ValidateChartOverridesForAssets(ctx context.Context, assets DeployAssets, charts map[string]string) error {
	_, err := chartOverrideReleases(assets, charts)
	if err != nil || len(charts) == 0 {
		return err
	}
	return admitChartExecutor(ctx, assets, sortedKeys(charts)[0])
}

// admitChartExecutor tests the exported executor's real dry-run contract on a
// one-item private deploy path. An old executor may prepend the deploy path to
// an absolute archive path; reject it before a site machine is taken.
func admitChartExecutor(ctx context.Context, assets DeployAssets, item string) error {
	if assets.Layout.Executor == "" {
		assets.Layout = CurrentLayout()
	}
	root, err := os.OpenRoot(assets.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if metadataErr := checkDeployMetadata(root, assets.Layout.Executor); metadataErr != nil {
		return metadataErr
	}
	release, err := deploypath.ReadRelease(filepath.Join(assets.Dir, assets.Layout.DeployPath, item))
	if err != nil {
		return err
	}
	const name = "nvke-e2e-chart-admission"
	const archive = "/nvke-e2e-chart-admission.tgz"
	release.Chart.Name, release.Chart.Version = name, "1.0.0"
	release.Chart.Path, release.Chart.Repository = archive, ""
	release.Chart.SetFile, release.Values, release.Requires = nil, nil, nil
	release.Release.Name, release.Release.Namespace = name, name
	probe, err := os.MkdirTemp("", "nvke-e2e-chart-admission-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probe)
	if directoryErr := os.Mkdir(filepath.Join(probe, "chart"), 0o700); directoryErr != nil {
		return directoryErr
	}
	if itemsErr := os.WriteFile(filepath.Join(probe, "nvke-deploy.yaml"), deploypath.MarshalItems([]deploypath.Item{{Name: "chart", Type: deploypath.TypeHelm}}, ""), 0o600); itemsErr != nil {
		return itemsErr
	}
	if releaseErr := os.WriteFile(filepath.Join(probe, "chart", "release.yaml"), release.Marshal(), 0o600); releaseErr != nil {
		return releaseErr
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, "bash", filepath.Join(assets.Dir, assets.Layout.Executor), "install", "--deploy-path", probe, "--dry-run") // #nosec G204 -- caller-selected executor is a bounded regular file under the export root; fixed argv, no shell interpolation.
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LC_ALL=C"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = time.Second
	raw, runErr := command.Output()
	if command.Process != nil {
		stopErr := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if stopErr != nil && !errors.Is(stopErr, syscall.ESRCH) {
			return fmt.Errorf("stop chart executor preflight: %w", stopErr)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			probeErr := syscall.Kill(-command.Process.Pid, 0)
			if errors.Is(probeErr, syscall.ESRCH) {
				break
			}
			if probeErr != nil {
				return fmt.Errorf("chart executor preflight process group did not stop: %w", probeErr)
			}
			if time.Now().After(deadline) {
				return errors.New("chart executor preflight process group did not stop before the cleanup deadline")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if runErr != nil {
		return fmt.Errorf("exported executor %s cannot admit a staged chart: %w", assets.Layout.Executor, errors.Join(runErr, probeCtx.Err()))
	}
	want := "helm upgrade --install " + name + " " + archive + " "
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, want) {
			return nil
		}
	}
	return fmt.Errorf("exported executor %s does not support an absolute packaged chart.path; use a newer deployment ref", assets.Layout.Executor)
}

func chartOverrideReleases(assets DeployAssets, charts map[string]string) (map[string][]byte, error) {
	if err := ValidateChartOverrides(charts); err != nil {
		return nil, err
	}
	releases := make(map[string][]byte, len(charts))
	if len(charts) == 0 {
		return releases, nil
	}
	if assets.Layout.Executor == "" {
		assets.Layout = CurrentLayout()
	}
	if !filepath.IsLocal(assets.Layout.DeployPath) {
		return nil, fmt.Errorf("deploy path %q is outside the exported assets", assets.Layout.DeployPath)
	}
	directory := filepath.Join(assets.Dir, assets.Layout.DeployPath)
	root, rootErr := os.OpenRoot(assets.Dir)
	if rootErr != nil {
		return nil, fmt.Errorf("open exported deploy assets: %w", rootErr)
	}
	defer root.Close()
	if err := checkDeployMetadata(root, path.Join(assets.Layout.DeployPath, "nvke-deploy.yaml")); err != nil {
		return nil, err
	}
	items, err := deploypath.Items(directory)
	if err != nil {
		return nil, fmt.Errorf("read exported deploy items: %w", err)
	}
	roles := make(map[string]string, len(items))
	for _, item := range items {
		if _, duplicate := roles[item.Name]; duplicate {
			return nil, fmt.Errorf("exported deploy path repeats item %q", item.Name)
		}
		roles[item.Name] = item.Type
	}
	for _, item := range sortedKeys(charts) {
		if roles[item] != deploypath.TypeHelm {
			return nil, fmt.Errorf("chart override %q is not a helm item in exported deploy path %s", item, assets.Layout.DeployPath)
		}
		if metadataErr := checkDeployMetadata(root, path.Join(assets.Layout.DeployPath, item, "release.yaml")); metadataErr != nil {
			return nil, metadataErr
		}
		release, readErr := deploypath.ReadRelease(filepath.Join(directory, item))
		if readErr != nil {
			return nil, fmt.Errorf("read exported release %s: %w", item, readErr)
		}
		if release.Chart == nil {
			return nil, fmt.Errorf("exported helm item %s has no chart", item)
		}
		release.Chart.Repository = ""
		release.Chart.Path = stagedChart(item)
		releases[item] = release.Marshal()
	}
	return releases, nil
}

func checkDeployMetadata(root *os.Root, relative string) error {
	info, err := root.Lstat(relative)
	if err != nil {
		return fmt.Errorf("exported deploy metadata %s: %w", relative, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 1<<20 {
		return fmt.Errorf("exported deploy metadata %s must be a bounded regular file", relative)
	}
	return nil
}

func stagedChart(item string) string { return path.Join(workDir, "charts", item+".tgz") }

// commands are the install and what the deploy path's README says surrounds
// it: untaint and clusterctl init and a default StorageClass before, the CAPI
// SPIFFE patch after.
func (d Stack) commands(t DeployTarget, valuesDir string) []step {
	kc := shellQuote(t.Cluster.NodeKubeconfig)
	env := "KUBECONFIG=" + kc + " "
	values := func(release string) string {
		return "--values-for " + shellQuote(release+"="+path.Join(valuesDir, valuesFile(release)))
	}
	defaultClass := `{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`
	// The flag that names the deploy path is owned by the selected layout.
	target := ""
	for i, arg := range d.Assets.Layout.TargetArgs(assetsDir) {
		if i > 0 {
			target += " "
		}
		target += shellQuote(arg)
	}
	return []step{
		{"untaint the control plane", env + "kubectl taint nodes --all node-role.kubernetes.io/control-plane- || true"},
		{"clusterctl init", env + "clusterctl init --wait-providers"},
		{"default storage class", env + "kubectl apply -f " + shellQuote(d.StorageClassManifest) +
			" && " + env + "kubectl patch storageclass " + shellQuote(d.StorageClassName) + " -p " + shellQuote(defaultClass)},
		{"install " + d.Assets.Layout.DeployPath, env + "bash " + shellQuote(path.Join(assetsDir, d.Assets.Layout.Executor)) + " install" +
			" " + target +
			" --config " + shellQuote(path.Join(valuesDir, "bootstrap-config.json")) +
			" " + values(releaseController) +
			" " + values(releaseSpire) +
			" " + values(releasePostgreSQL) +
			" " + values(releaseTemporal) +
			" --kubeconfig " + kc},
		{"CAPI SPIFFE exec auth", env + "bash " + shellQuote(path.Join(assetsDir, hackDir, "apply-capi-spiffe-exec-auth.sh"))},
	}
}

// render produces the per-site values files and the BootstrapConfig the
// deploy path's template items render against.
func (d Stack) render(t DeployTarget, spireOverlay []byte) (map[string][]byte, error) {
	vip := t.Node.IP
	domains, err := oidcDomains(spireOverlay, vip)
	if err != nil {
		return nil, err
	}

	checksum, _ := tpmServerChecksum(d.Arch)

	// The overlay's env list is replaced, not merged, so what it sets is
	// repeated here.
	env := []envVar{
		{Name: "NODE_AGENT_PORT", Value: nodeAgentPort},
		{Name: "SPIRE_SERVER_API_ADDRESS", Value: spireServerAPIAddress},
	}
	for _, k := range sortedKeys(d.ControllerEnv) {
		env = append(env, envVar{Name: k, Value: d.ControllerEnv[k]})
	}
	// One value, three places, under two different keys. Temporal's datastores
	// take sql.password; the Bitnami chart takes auth.postgresPassword, and
	// only falls back to auth.password when auth.username is postgres -- so
	// auth.password renders a randomly generated superuser password the moment
	// the deploy path gives CWE and Temporal an account of their own.
	sqlPassword := map[string]any{"password": d.DatabasePassword}

	docs := map[string]any{
		valuesFile(releaseController): map[string]any{
			"image":       d.Controller.values(),
			"appDb":       map[string]any{"postgresPassword": d.DatabasePassword},
			"nkeOIDCAuth": map[string]any{"discoveryURL": fmt.Sprintf("https://%s:%d/.well-known/openid-configuration", vip, oidcPort)},
			"spiffe":      map[string]any{"trustDomain": t.TrustDomain, "spire": map[string]any{"serverAddress": vip}},
			"nodeAgent":   map[string]any{"spireAttestationMethod": "tpm"},
			"profiles":    map[string]any{"image": d.Profiles.values()},
			"env":         env,
		},
		valuesFile(releaseSpire): map[string]any{
			"global": map[string]any{"spire": map[string]any{"trustDomain": t.TrustDomain, "clusterName": t.SiteName}},
			"spire-server": map[string]any{
				"adminIDs":     []string{"spiffe://" + t.TrustDomain + spireAdminPath()},
				"nodeAttestor": map[string]any{"tpmDirect": map[string]any{"checksum": checksum}},
			},
			"spiffe-oidc-discovery-provider": map[string]any{
				"config": map[string]any{
					"jwksUri":           fmt.Sprintf("https://%s:%d/keys", vip, oidcPort),
					"additionalDomains": domains,
				},
				"tls": map[string]any{"certManager": map[string]any{"certificate": map[string]any{"ipAddresses": []string{vip}}}},
			},
		},
		valuesFile(releasePostgreSQL): map[string]any{
			"auth": map[string]any{"postgresPassword": d.DatabasePassword},
		},
		valuesFile(releaseTemporal): map[string]any{
			"server": map[string]any{"config": map[string]any{"persistence": map[string]any{
				"default":    map[string]any{"sql": sqlPassword},
				"visibility": map[string]any{"sql": sqlPassword},
			}}},
		},
	}

	files := make(map[string][]byte, len(docs)+1)
	for name, doc := range docs {
		data, merr := yaml.Marshal(doc)
		if merr != nil {
			return nil, fmt.Errorf("render %s: %w", name, merr)
		}
		files[name] = data
	}
	// ClusterName is the name NKTP labels the site's telemetry with. NVKE's
	// kubeadm configuration names every cluster "kubernetes".
	config, err := json.MarshalIndent(map[string]any{
		d.Provider.ConfigKey(): d.Provider,
		"Registry":             d.Registry,
		"Tenant":               d.Tenant,
		"ClusterName":          t.SiteName,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render bootstrap config: %w", err)
	}
	files["bootstrap-config.json"] = config
	return files, nil
}

// oidcDomains is the overlay's additionalDomains plus the VIP. A values file
// replaces a list outright, so the list passed has to be complete, and it is
// read from the overlay rather than repeated here so the two cannot drift.
func oidcDomains(overlay []byte, vip string) ([]string, error) {
	var doc struct {
		Provider struct {
			Config struct {
				AdditionalDomains []string `json:"additionalDomains"`
			} `json:"config"`
		} `json:"spiffe-oidc-discovery-provider"`
	}
	if err := yaml.Unmarshal(overlay, &doc); err != nil {
		return nil, fmt.Errorf("parse SPIRE overlay: %w", err)
	}
	domains := doc.Provider.Config.AdditionalDomains
	if len(domains) == 0 {
		return nil, errors.New("the SPIRE overlay declares no OIDC additionalDomains; " +
			"passing only the VIP would drop the static hostnames")
	}
	for _, d := range domains {
		if d == vip {
			return domains, nil
		}
	}
	return append(domains, vip), nil
}

func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate database password: %w", err)
	}
	return hex.EncodeToString(b), nil
}
