// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// nke-k8s-installer installs the Kubernetes node stack from a bundle tree that
// is already on disk. The nke-kubeadm-distro-<x.y.z> package runs it from its
// postinst with NKE_BUNDLE_ROOT pointing at /usr/lib/nke/k8s-matrix/<version>.
// It never downloads anything.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/bundle"
)

const (
	// targetArch is the only architecture NKE nodes run.
	targetArch = "amd64"
	// defaultPauseSandboxFallback is used when kubeadm cannot report the pause
	// image for this release.
	defaultPauseSandboxFallback = "registry.k8s.io/pause:3.10"
	// workRoot is the parent of the scratch directory tarballs are unpacked in.
	// It is under /var/lib rather than /tmp because root extracts archives
	// there, and /tmp is writable by every user on the node.
	workRoot = "/var/lib/nke"
)

// manifest is the subset of the bundle manifest.json the installer consumes.
type manifest struct {
	Architectures map[string]map[string]component `json:"architectures"`
}

// component is one artifact in the bundle: its filename and expected digest.
type component struct {
	File   string `json:"file"`
	Sha256 string `json:"sha256"`
}

// installer installs one Kubernetes release from one bundle directory.
type installer struct {
	version    string
	bundleRoot string
	workDir    string
	manifest   manifest
}

func main() {
	version := flag.String("version", "", "Kubernetes version to install, for example v1.36.3. Also accepted as a positional argument.")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	v := *version
	if v == "" && flag.NArg() > 0 {
		v = flag.Arg(0)
	}
	if err := run(v); err != nil {
		slog.Error("installation failed", "err", err)
		os.Exit(1)
	}
}

func run(version string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root")
	}
	if strings.TrimSpace(version) == "" {
		return fmt.Errorf("no Kubernetes version given (-version or first argument)")
	}
	bundleRoot := strings.TrimSpace(os.Getenv("NKE_BUNDLE_ROOT"))
	if bundleRoot == "" {
		return fmt.Errorf("NKE_BUNDLE_ROOT is not set; it must point at the bundle directory holding manifest.json")
	}
	i := &installer{version: normalizeVersion(version), bundleRoot: filepath.Clean(bundleRoot)}

	workDir, err := makeWorkDir()
	if err != nil {
		return err
	}
	i.workDir = workDir
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			slog.Warn("could not remove the scratch directory", "dir", workDir, "err", err)
		}
	}()

	if err := i.prepareOS(); err != nil {
		return fmt.Errorf("preparing OS: %w", err)
	}
	if err := i.loadManifest(); err != nil {
		return fmt.Errorf("loading bundle manifest: %w", err)
	}
	if err := i.deployBinaries(); err != nil {
		return fmt.Errorf("deploying binaries: %w", err)
	}
	if err := i.configureServices(); err != nil {
		return fmt.Errorf("configuring services: %w", err)
	}
	slog.Info("installed Kubernetes node stack", "version", i.version, "arch", targetArch)
	slog.Info("kubelet is enabled but not started; kubeadm init or join starts it")
	return nil
}

// makeWorkDir creates the private 0700 scratch directory archives are unpacked
// in, under a root-owned parent.
func makeWorkDir() (string, error) {
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", workRoot, err)
	}
	dir, err := os.MkdirTemp(workRoot, "install-")
	if err != nil {
		return "", fmt.Errorf("create a scratch directory under %s: %w", workRoot, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("chmod %s: %w", dir, err)
	}
	return dir, nil
}

// normalizeVersion returns the leading-v form the bundle directories use.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// runCommand runs cmd, logging it, and wraps a failure with its combined output.
func runCommand(name string, args ...string) error {
	slog.Info("running", "cmd", name, "args", args)
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// prepareOS disables swap, loads the kernel modules kube-proxy and the CNI need,
// and sets the sysctls kubeadm's preflight checks require.
func (i *installer) prepareOS() error {
	slog.Info("preparing OS: swap, kernel modules, sysctl")

	if err := disableSwap(); err != nil {
		return err
	}

	if err := os.WriteFile("/etc/modules-load.d/k8s.conf", []byte("overlay\nbr_netfilter\nnf_conntrack\n"), 0o644); err != nil {
		return fmt.Errorf("write /etc/modules-load.d/k8s.conf: %w", err)
	}
	for _, mod := range []string{"overlay", "br_netfilter"} {
		if err := runCommand("modprobe", mod); err != nil {
			return err
		}
	}
	// Tolerated: nf_conntrack is compiled into some kernels, where modprobe has
	// no module to load and fails even though conntrack works.
	if err := runCommand("modprobe", "nf_conntrack"); err != nil {
		slog.Warn("could not load nf_conntrack; it may be built into this kernel", "err", err)
	}

	// kubeadm's preflight requires IPv6 forwarding as well as IPv4 since 1.34
	// (FileContent--proc-sys-net-ipv6-conf-default-forwarding is fatal).
	sysctlConf := "net.bridge.bridge-nf-call-iptables  = 1\n" +
		"net.bridge.bridge-nf-call-ip6tables = 1\n" +
		"net.ipv4.ip_forward                 = 1\n" +
		"net.ipv6.conf.all.forwarding        = 1\n" +
		"net.ipv6.conf.default.forwarding    = 1\n"
	if err := os.WriteFile("/etc/sysctl.d/k8s.conf", []byte(sysctlConf), 0o644); err != nil {
		return fmt.Errorf("write /etc/sysctl.d/k8s.conf: %w", err)
	}
	return runCommand("sysctl", "--system")
}

// disableSwap turns swap off now and keeps it off across reboots: kubelet refuses
// to run with swap enabled unless it is explicitly configured for it.
func disableSwap() error {
	if err := runCommand("swapoff", "-a"); err != nil {
		return err
	}
	if err := commentOutFstabSwap("/etc/fstab"); err != nil {
		return err
	}
	if err := os.MkdirAll("/etc/systemd", 0o755); err != nil {
		return fmt.Errorf("create /etc/systemd: %w", err)
	}
	if err := os.WriteFile("/etc/systemd/zram-generator.conf", []byte("[zram0]\nzram-size = 0\n"), 0o644); err != nil {
		return fmt.Errorf("write /etc/systemd/zram-generator.conf: %w", err)
	}
	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		return err
	}
	// Tolerated: swap.target is a static unit on some systemd versions, which
	// refuse to mask it. swapoff plus the fstab edit have already done the work.
	if err := runCommand("systemctl", "mask", "--now", "swap.target"); err != nil {
		slog.Warn("could not mask swap.target; swap is already off", "err", err)
	}
	return nil
}

// commentOutFstabSwap comments out every swap entry in an fstab file. A missing
// file is not an error: not every image ships one.
func commentOutFstabSwap(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(content), "\n")
	changed := false
	for n, line := range lines {
		stripped := strings.TrimLeft(line, " \t")
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		if fields := strings.Fields(line); len(fields) >= 3 && fields[2] == "swap" {
			lines[n] = "#" + line
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// loadManifest reads manifest.json from the bundle root after checking it against
// its digest sidecar, and validates the components it declares.
func (i *installer) loadManifest() error {
	manifestPath, err := bundle.JoinUnderRoot(i.bundleRoot, "manifest.json")
	if err != nil {
		return err
	}
	if err := bundle.VerifyManifestDigest(manifestPath); err != nil {
		return fmt.Errorf("manifest integrity: %w", err)
	}
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, &i.manifest); err != nil {
		return fmt.Errorf("parse %s: %w", manifestPath, err)
	}

	components, ok := i.manifest.Architectures[targetArch]
	if !ok || len(components) == 0 {
		known := make([]string, 0, len(i.manifest.Architectures))
		for a := range i.manifest.Architectures {
			known = append(known, a)
		}
		sort.Strings(known)
		return fmt.Errorf("manifest has no components for architecture %q (have %v)", targetArch, known)
	}
	for name, info := range components {
		if err := bundle.ValidateComponentName(name); err != nil {
			return err
		}
		if err := bundle.ValidateArtifactBasename(info.File); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// deployBinaries verifies and installs every component the manifest declares,
// skipping the ones already present at the expected digest.
func (i *installer) deployBinaries() error {
	components := i.manifest.Architectures[targetArch]
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		info := components[name]
		want := strings.ToLower(strings.TrimSpace(info.Sha256))
		if target := targetPath(name); want != "" && target != "" {
			if got, err := bundle.FileSHA256(target); err == nil && got == want {
				slog.Info("component already up to date", "component", name)
				continue
			}
		}
		artifact, err := i.verifiedArtifact(name, info)
		if err != nil {
			return err
		}
		slog.Info("installing component", "component", name)
		if err := i.installComponent(name, artifact); err != nil {
			return fmt.Errorf("install %s: %w", name, err)
		}
	}
	return nil
}

// verifiedArtifact resolves a component's file inside the bundle and verifies it
// against both its checksum sidecar and the manifest digest.
func (i *installer) verifiedArtifact(name string, info component) (string, error) {
	archDir, err := bundle.JoinUnderRoot(i.bundleRoot, targetArch)
	if err != nil {
		return "", err
	}
	path, err := bundle.JoinUnderRoot(archDir, info.File)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%s: bundled artifact missing at %s: %w", name, path, err)
	}
	digest, err := bundle.VerifyFileAgainstSidecar(path, path+".sha256")
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if want := strings.ToLower(strings.TrimSpace(info.Sha256)); want != "" && want != digest {
		return "", fmt.Errorf("%s: manifest sha256 does not match the checksum sidecar", name)
	}
	return path, nil
}

// targetPath is where a component ends up on the node, for components installed
// as a single binary. Archives return "".
func targetPath(name string) string {
	switch name {
	case "kubelet", "kubeadm", "kubectl", "helm", "crictl":
		return "/usr/bin/" + name
	case "runc":
		return "/usr/sbin/runc"
	default:
		return ""
	}
}

// installComponent puts one verified artifact in its place on the node.
func (i *installer) installComponent(name, source string) error {
	switch name {
	case "kubelet", "kubeadm", "kubectl":
		return installBinary(source, "/usr/bin/"+name)
	case "runc":
		return installBinary(source, "/usr/sbin/runc")
	case "containerd":
		return runCommand("tar", "-C", "/usr/local", "-xzf", source)
	case "cni-plugins":
		if err := os.MkdirAll("/opt/cni/bin", 0o755); err != nil {
			return fmt.Errorf("create /opt/cni/bin: %w", err)
		}
		return runCommand("tar", "-C", "/opt/cni/bin", "-xzf", source)
	case "helm":
		unpacked, err := i.unpackTo(source, "helm")
		if err != nil {
			return err
		}
		return installBinary(filepath.Join(unpacked, "linux-"+targetArch, "helm"), "/usr/bin/helm")
	case "crictl":
		unpacked, err := i.unpackTo(source, "crictl")
		if err != nil {
			return err
		}
		return installBinary(filepath.Join(unpacked, "crictl"), "/usr/bin/crictl")
	default:
		return fmt.Errorf("unsupported component %q", name)
	}
}

// unpackTo extracts a tarball into a fresh directory under the scratch directory.
func (i *installer) unpackTo(source, name string) (string, error) {
	dir := filepath.Join(i.workDir, name+"-unpack")
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clean %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	if err := runCommand("tar", "-C", dir, "-xzf", source); err != nil {
		return "", err
	}
	return dir, nil
}

// installBinary copies src over dst and makes it executable.
func installBinary(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Replace rather than truncate: the file may be a running binary.
	tmp := dst + ".nke-tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, tmp, err)
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("install %s: %w", dst, err)
	}
	return nil
}

// The sandbox image moved between containerd's config schemas. Version 2
// (containerd 1.x) has `sandbox_image = "..."` under the CRI plugin; version 3
// (containerd 2.x, what every pin in this bundle emits) has `sandbox = '...'`
// under [plugins.'io.containerd.cri.v1.images'.pinned_images]. Quoting is
// accepted either way, because TOML does not care and containerd has changed
// which it emits.
var (
	sandboxImageV2Pattern = regexp.MustCompile(`(?m)^([ \t]*)sandbox_image[ \t]*=[ \t]*["'][^"']*["']`)
	sandboxImageV3Pattern = regexp.MustCompile(`(?m)^([ \t]*)sandbox[ \t]*=[ \t]*["'][^"']*["']`)
	// runcOptionsTablePattern matches the runc options table header in both
	// schemas: the plugin id differs, the tail does not.
	runcOptionsTablePattern = regexp.MustCompile(`(?m)^([ \t]*)\[plugins\..*\.runtimes\.runc\.options\][ \t]*$`)
)

// rewriteContainerdConfig points containerd's sandbox image at the one kubeadm
// expects and puts the runtime on the systemd cgroup driver, which kubelet
// requires. It fails rather than writing a configuration that silently kept the
// upstream defaults.
func rewriteContainerdConfig(config, pauseImage string) (string, error) {
	// The two patterns are anchored on the whole key, so `sandbox_image` cannot
	// match the v3 pattern and the counts stay independent. Either schema is
	// accepted; at least one has to match.
	v2 := len(sandboxImageV2Pattern.FindAllString(config, -1))
	v3 := len(sandboxImageV3Pattern.FindAllString(config, -1))
	config = sandboxImageV2Pattern.ReplaceAllString(config, fmt.Sprintf(`${1}sandbox_image = "%s"`, pauseImage))
	config = sandboxImageV3Pattern.ReplaceAllString(config, fmt.Sprintf(`${1}sandbox = '%s'`, pauseImage))
	if v2+v3 == 0 {
		return "", fmt.Errorf("no sandbox image setting in the containerd configuration: " +
			"expected sandbox_image (config version 2) or sandbox (config version 3)")
	}

	config = strings.ReplaceAll(config, "SystemdCgroup = false", "SystemdCgroup = true")
	if strings.Contains(config, "SystemdCgroup = true") {
		return config, nil
	}
	// No SystemdCgroup key at all: add one to the runc options table rather than
	// leave the runtime on the cgroupfs driver.
	loc := runcOptionsTablePattern.FindStringSubmatchIndex(config)
	if loc == nil {
		return "", fmt.Errorf("containerd configuration has no SystemdCgroup setting and no runc options table to add one to")
	}
	indent := config[loc[2]:loc[3]] + "  "
	return config[:loc[1]] + "\n" + indent + "SystemdCgroup = true" + config[loc[1]:], nil
}

// configureServices writes the containerd configuration and the systemd units,
// then enables containerd and kubelet.
func (i *installer) configureServices() error {
	slog.Info("configuring containerd and systemd units")

	if err := os.MkdirAll("/etc/containerd", 0o755); err != nil {
		return fmt.Errorf("create /etc/containerd: %w", err)
	}
	defaults, err := exec.Command("/usr/local/bin/containerd", "config", "default").Output()
	if err != nil {
		return fmt.Errorf("containerd config default: %w", err)
	}
	pauseImage, err := pauseImageFromKubeadm(i.version)
	if err != nil {
		slog.Warn("could not resolve the pause image with kubeadm; using the fallback",
			"err", err, "fallback", defaultPauseSandboxFallback)
		pauseImage = defaultPauseSandboxFallback
	}
	config, err := rewriteContainerdConfig(string(defaults), pauseImage)
	if err != nil {
		return err
	}
	if err := os.WriteFile("/etc/containerd/config.toml", []byte(config), 0o644); err != nil {
		return fmt.Errorf("write /etc/containerd/config.toml: %w", err)
	}

	if err := writeUnits(); err != nil {
		return err
	}

	const kubeletDefaults = "/etc/default/kubelet"
	if _, err := os.Stat(kubeletDefaults); os.IsNotExist(err) {
		content := "# Optional kubelet flags; kubeadm uses /var/lib/kubelet/kubeadm-flags.env.\nKUBELET_EXTRA_ARGS=\n"
		if err := os.WriteFile(kubeletDefaults, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", kubeletDefaults, err)
		}
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", kubeletDefaults, err)
	}

	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCommand("systemctl", "enable", "containerd"); err != nil {
		return err
	}
	// restart, not `enable --now`: on a re-install containerd is already running
	// with the previous config.toml, and --now would leave it there.
	if err := runCommand("systemctl", "restart", "containerd"); err != nil {
		return err
	}
	// kubelet is enabled but not started: kubeadm's preflight check wants port
	// 10250 free until `kubeadm init` or `kubeadm join` runs kubelet-start.
	return runCommand("systemctl", "enable", "kubelet")
}

// pauseImageFromKubeadm asks the installed kubeadm which pause image this release
// expects, so containerd's sandbox image matches the cluster's.
func pauseImageFromKubeadm(k8sVersion string) (string, error) {
	out, err := exec.Command("/usr/bin/kubeadm", "config", "images", "list", "--kubernetes-version", k8sVersion).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubeadm config images list: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parsePauseImage(string(out))
}

// parsePauseImage picks the pause image out of `kubeadm config images list`.
func parsePauseImage(out string) (string, error) {
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if len(strings.Fields(line)) != 1 {
			continue
		}
		if strings.Contains(line, "/pause:") || strings.HasPrefix(line, "pause:") {
			return line, nil
		}
	}
	return "", fmt.Errorf("no pause image in kubeadm output")
}

// containerdUnit follows containerd's own recommended unit.
const containerdUnit = `[Unit]
Description=containerd container runtime
Documentation=https://containerd.io
After=network.target local-fs.target
Wants=network-online.target
After=network-online.target

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
`

// kubeletUnit is only written when the package's vendor unit is absent, because
// a unit in /etc/systemd/system masks the one in /lib.
const kubeletUnit = `[Unit]
Description=kubelet: The Kubernetes Node Agent
Documentation=https://kubernetes.io/docs/reference/command-line-tools-reference/kubelet/
Wants=network-online.target
After=network-online.target containerd.service
Requires=containerd.service

[Service]
Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
EnvironmentFile=-/etc/default/kubelet
ExecStart=
ExecStart=/usr/bin/kubelet $KUBELET_KUBECONFIG_ARGS $KUBELET_CONFIG_ARGS $KUBELET_KUBEADM_ARGS $KUBELET_EXTRA_ARGS
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`

// writeUnits installs the containerd unit, and a kubelet unit only when the
// package did not already ship the vendor unit plus kubeadm drop-in under /lib.
func writeUnits() error {
	if err := os.WriteFile("/etc/systemd/system/containerd.service", []byte(containerdUnit), 0o644); err != nil {
		return fmt.Errorf("write containerd.service: %w", err)
	}
	if packagedKubeletUnits() {
		return nil
	}
	if err := os.WriteFile("/etc/systemd/system/kubelet.service", []byte(kubeletUnit), 0o644); err != nil {
		return fmt.Errorf("write kubelet.service: %w", err)
	}
	return nil
}

// packagedKubeletUnits reports whether an nke-kubeadm-distro package installed
// the pkgs.k8s.io-style split unit: vendor kubelet.service plus 10-kubeadm.conf.
func packagedKubeletUnits() bool {
	if _, err := os.Stat("/lib/systemd/system/kubelet.service"); err != nil {
		return false
	}
	_, err := os.Stat("/lib/systemd/system/kubelet.service.d/10-kubeadm.conf")
	return err == nil
}
