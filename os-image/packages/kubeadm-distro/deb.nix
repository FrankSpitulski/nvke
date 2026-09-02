# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# One nke-kubeadm-distro-<x.y.z>_<rev>_amd64.deb, built entirely from upstream
# release binaries pinned by sha256 in versions.json. Nothing is compiled here:
# fetchurl brings down the same tarballs a human would, dockerTools.pullImage
# brings down the control-plane images by linux/amd64 digest, and fpm turns the
# staged tree into a .deb.
#
# The Kubernetes patch is part of the package *name*; the Debian Version field
# is only the packaging revision.
{
  pkgs,
  version, # "v1.36.3"
  spec, # the versions.json entry for that version
  peers, # every version in versions.json, for Conflicts
  postinstTemplate,
  debRevision ? "1",
}:
let
  lib = pkgs.lib;

  arch = "amd64";
  packagePrefix = "nke-kubeadm-distro";
  matrixSubdir = "k8s-matrix";

  patch = lib.removePrefix "v" version;
  packageName = "${packagePrefix}-${patch}";
  debFileName = "${packageName}_${debRevision}_${arch}.deb";
  imageTarball = "k8s-base-container-images_${patch}_linux_${arch}.tgz";

  artifact = name: pkgs.fetchurl { inherit (spec.artifacts.${name}) url sha256; };

  # A ref is always registry/name:tag with no port, so splitting on ":" is
  # unambiguous. Anything else is a lock defect and should stop the build.
  refName =
    ref:
    let
      parts = lib.splitString ":" ref;
    in
    if builtins.length parts == 2 then builtins.head parts else throw "cannot split image ref ${ref}";
  refTag = ref: lib.last (lib.splitString ":" ref);

  pauseRef =
    let
      matches = builtins.filter (i: lib.hasInfix "/pause:" i.ref) spec.images;
    in
    if matches == [ ] then
      throw "no pause image in the ${version} image list; containerd has no sandbox image to pin"
    else
      (builtins.head matches).ref;

  pulledImages = map (
    i:
    pkgs.dockerTools.pullImage {
      imageName = refName i.ref;
      imageDigest = i.digest;
      # The registry digest names the image; this is the nix output hash of the
      # archive skopeo writes for it, which is a different thing entirely.
      sha256 = i.hash;
      finalImageName = refName i.ref;
      finalImageTag = refTag i.ref;
      os = "linux";
      arch = arch;
    }
  ) spec.images;

  # One gzipped docker-archive holding every control-plane image, under the
  # exact name postinst imports. Blobs are re-keyed by their own sha256 so two
  # images that share a layer ship it once and two that do not cannot collide.
  imageArchive =
    pkgs.runCommand "k8s-base-container-images-${patch}"
      {
        nativeBuildInputs = [
          pkgs.jq
          pkgs.gnutar
          pkgs.gzip
        ];
        tars = pulledImages;
      }
      ''
        mkdir -p merged/blobs parts
        n=0
        for t in $tars; do
          ex="ex$n"
          mkdir "$ex"
          tar -xf "$t" -C "$ex"
          chmod -R u+w "$ex"
          rm -f "$ex/repositories"

          : >"$ex.map"
          while IFS= read -r f; do
            h="$(sha256sum "$ex/$f" | cut -d' ' -f1)"
            [ -e "merged/blobs/$h" ] || cp "$ex/$f" "merged/blobs/$h"
            printf '%s\t%s\n' "$f" "blobs/$h" >>"$ex.map"
          done < <(cd "$ex" && find . -type f ! -name manifest.json -printf '%P\n' | sort)

          jq -R -s 'split("\n") | map(select(length > 0) | split("\t"))
                    | map({key: .[0], value: .[1]}) | from_entries' \
            "$ex.map" >"$ex.mapjson"
          # error(), not a null lookup: a manifest naming a path the map does
          # not hold means the extraction and the rewrite disagree about this
          # archive's layout, and a null Config would only surface much later,
          # on the node, as an archive ctr refuses to import.
          jq --slurpfile m "$ex.mapjson" \
            '[.[]
              | .Config = ($m[0][.Config] // error("no blob for \(.Config)"))
              | .Layers = [.Layers[] | . as $l | ($m[0][$l] // error("no blob for \($l)"))]]' \
            "$ex/manifest.json" >"parts/$n.json"
          n=$((n + 1))
        done

        jq -s 'add' parts/*.json >merged/manifest.json

        # --sort=name plus a fixed mtime and owner: the archive is a build
        # output that has to be reproducible, and gzip -n keeps the timestamp
        # out of the gzip header too. Level 6 (gzip's default) rather than 9:
        # the layers are uncompressed tars, so compression is worth doing, but
        # 9 costs several times the CPU for about a percent of size.
        #
        # --no-recursion because the file list already names every member: it
        # holds `blobs` *and* each blob under it, and tar would otherwise walk
        # the directory as well and archive every blob a second time.
        (cd merged && find . -mindepth 1 -printf '%P\n' | sort >../filelist)
        mkdir -p $out
        tar -C merged --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1 \
          --no-recursion -cf - -T filelist | gzip -n -6 >"$out/${imageTarball}"

        # A member listed twice is what a missing --no-recursion looks like,
        # and it is invisible from the outside: the archive still imports,
        # just at twice the size. Fail the build instead.
        dups="$(tar -tzf "$out/${imageTarball}" | sort | uniq -d)"
        if [ -n "$dups" ]; then
          echo "duplicate members in ${imageTarball}:" >&2
          echo "$dups" >&2
          exit 1
        fi

        (cd $out && sha256sum "${imageTarball}" >"${imageTarball}.sha256")
      '';

  # containerd 2.x reads config version 3. Unset keys keep each plugin's own
  # defaults, so only what has to differ from stock is written here.
  containerdConfig = pkgs.writeText "containerd-config.toml" ''
    # Managed by ${packageName}. Rendered at package build time from
    # versions.json; the sandbox image is the pause image this Kubernetes
    # release asks for, so it can never skew from the bundled images.
    version = 3

    [plugins]
      [plugins.'io.containerd.cri.v1.images']
        [plugins.'io.containerd.cri.v1.images'.pinned_images]
          sandbox = '${pauseRef}'
      [plugins.'io.containerd.cri.v1.runtime']
        [plugins.'io.containerd.cri.v1.runtime'.cni]
          bin_dir = '/opt/cni/bin'
          conf_dir = '/etc/cni/net.d'
        [plugins.'io.containerd.cri.v1.runtime'.containerd]
          default_runtime_name = 'runc'
          [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes]
            [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
              runtime_type = 'io.containerd.runc.v2'
              [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]
                BinaryName = '/usr/sbin/runc'
                SystemdCgroup = true
  '';

  crictlConfig = pkgs.writeText "crictl.yaml" ''
    runtime-endpoint: unix:///run/containerd/containerd.sock
    image-endpoint: unix:///run/containerd/containerd.sock
    timeout: 10
  '';

  # containerd's own recommended unit, with the /usr/local/bin path this
  # package installs to.
  containerdUnit = pkgs.writeText "containerd.service" ''
    [Unit]
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
    LimitNPROC=infinity
    LimitCORE=infinity
    TasksMax=infinity
    OOMScoreAdjust=-999

    [Install]
    WantedBy=multi-user.target
  '';

  # The vendor kubelet unit plus the kubeadm drop-in, the same split the
  # upstream kubelet and kubeadm packages ship.
  kubeletUnit = pkgs.writeText "kubelet.service" ''
    [Unit]
    Description=kubelet: The Kubernetes Node Agent
    Documentation=https://kubernetes.io/docs/
    Wants=network-online.target
    After=network-online.target

    [Service]
    ExecStart=/usr/bin/kubelet
    Restart=always
    StartLimitInterval=0
    RestartSec=10

    [Install]
    WantedBy=multi-user.target
  '';

  kubeadmDropin = pkgs.writeText "10-kubeadm.conf" ''
    # Note: This dropin only works with kubeadm and kubelet v1.11+
    # Source: Kubernetes kubeadm deb (pkgs.k8s.io); paths use /usr/bin/kubelet.
    [Service]
    Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
    Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml"
    # This is a file that "kubeadm init" and "kubeadm join" generates at runtime, populating the KUBELET_KUBEADM_ARGS variable dynamically
    EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
    # This is a file that the user can use for overrides of the kubelet args as a last resort. Preferably, the user should use
    # the .NodeRegistration.KubeletExtraArgs object in the configuration files instead. KUBELET_EXTRA_ARGS should be sourced from this file.
    EnvironmentFile=-/etc/default/kubelet
    ExecStart=
    ExecStart=/usr/bin/kubelet $KUBELET_KUBECONFIG_ARGS $KUBELET_CONFIG_ARGS $KUBELET_KUBEADM_ARGS $KUBELET_EXTRA_ARGS
  '';

  kubeletDefaults = pkgs.writeText "kubelet-defaults" ''
    # Optional kubelet flags; kubeadm uses /var/lib/kubelet/kubeadm-flags.env.
    KUBELET_EXTRA_ARGS=
  '';

  # What this row was built from, readable on the node without unpacking
  # anything. It is a record, not an input: nothing reads it back at install
  # time, because dpkg already owns every file it describes.
  bundleManifest = pkgs.writeText "manifest.json" (
    builtins.toJSON {
      kubernetes = version;
      architecture = arch;
      package = packageName;
      components = spec.components;
      artifacts = spec.artifacts;
      images = map (i: { inherit (i) ref digest; }) spec.images;
      containerImageArchive = "${arch}/${imageTarball}";
    }
  );

  # Every matrix row owns /usr/bin/kubeadm, so each package conflicts with the
  # virtual name and with every other row -- every row in versions.json, not
  # just the ones this invocation builds.
  peerConflicts = [
    packagePrefix
  ]
  ++ map (p: "${packagePrefix}-${lib.removePrefix "v" p}") (
    builtins.filter (p: lib.removePrefix "v" p != patch) peers
  );

  # Upstream packages this one replaces wholesale. Installing both would leave
  # two owners of /usr/bin/kubeadm.
  upstreamConflicts = [
    "kubeadm"
    "kubelet"
    "kubectl"
    "kubernetes-cni"
    "cri-tools"
    "runc"
    "helm"
  ];

  description = ''
    NKE kubeadm node stack (Kubernetes ${version})
    Bundled Kubernetes stack with pkgs.k8s.io-style paths: /usr/bin/kubeadm,
    kubectl, kubelet; /usr/sbin/runc; containerd and ctr under /usr/local/bin;
    CNI plugins under /opt/cni/bin; kubelet systemd units under
    /lib/systemd/system. Every binary is the upstream release artifact, pinned
    by sha256. The control-plane container images for this release ship
    alongside under /usr/lib/nke/${matrixSubdir}/${version}/, and postinst
    imports them into containerd, so a node can come up with no registry
    access at all. Do not install alongside the upstream kubeadm, kubelet or
    CNI packages.
  '';
in
pkgs.runCommand debFileName
  {
    nativeBuildInputs = [
      pkgs.fpm
      pkgs.binutils
      pkgs.gnutar
      pkgs.gzip
      pkgs.jq
    ];
    passthru = { inherit debFileName packageName imageArchive; };
    meta.description = "NKE kubeadm node stack for Kubernetes ${version} (amd64 .deb)";
  }
  ''
    export HOME="$NIX_BUILD_TOP"
    root="$NIX_BUILD_TOP/root"

    # dpkg records the package root's own mode as the "./" member. mkdir -p
    # here rather than mktemp -d, whose 0700 would ship an unreadable /.
    mkdir -p "$root"
    chmod 0755 "$root"

    mkdir -p "$root/usr/bin" "$root/usr/sbin" "$root/usr/local/bin" "$root/opt/cni/bin"
    install -m 0755 ${artifact "kubeadm"} "$root/usr/bin/kubeadm"
    install -m 0755 ${artifact "kubectl"} "$root/usr/bin/kubectl"
    install -m 0755 ${artifact "kubelet"} "$root/usr/bin/kubelet"
    install -m 0755 ${artifact "runc"} "$root/usr/sbin/runc"

    tar -xzf ${artifact "cni-plugins"} -C "$root/opt/cni/bin"

    mkdir -p unpack/containerd unpack/helm unpack/crictl
    tar -xzf ${artifact "containerd"} -C unpack/containerd
    for b in containerd containerd-shim-runc-v2 ctr; do
      install -m 0755 "unpack/containerd/bin/$b" "$root/usr/local/bin/$b"
    done
    tar -xzf ${artifact "helm"} -C unpack/helm
    install -m 0755 unpack/helm/linux-${arch}/helm "$root/usr/bin/helm"
    tar -xzf ${artifact "crictl"} -C unpack/crictl
    install -m 0755 unpack/crictl/crictl "$root/usr/bin/crictl"

    mkdir -p "$root/etc/containerd" "$root/etc/default"
    install -m 0644 ${containerdConfig} "$root/etc/containerd/config.toml"
    install -m 0644 ${crictlConfig} "$root/etc/crictl.yaml"
    install -m 0644 ${kubeletDefaults} "$root/etc/default/kubelet"

    mkdir -p "$root/lib/systemd/system/kubelet.service.d"
    install -m 0644 ${containerdUnit} "$root/lib/systemd/system/containerd.service"
    install -m 0644 ${kubeletUnit} "$root/lib/systemd/system/kubelet.service"
    install -m 0644 ${kubeadmDropin} "$root/lib/systemd/system/kubelet.service.d/10-kubeadm.conf"

    # These two ship empty on purpose: kubeadm's DirAvailable preflight check
    # reads one name and calls any entry, hidden or not, "not empty".
    mkdir -p "$root/etc/kubernetes/manifests" "$root/var/lib/kubelet"

    matrix="$root/usr/lib/nke/${matrixSubdir}/${version}"
    mkdir -p "$matrix/${arch}"
    jq -S --indent 2 . ${bundleManifest} >"$matrix/manifest.json"
    chmod 0644 "$matrix/manifest.json"
    install -m 0644 ${imageArchive}/${imageTarball} "$matrix/${arch}/${imageTarball}"
    install -m 0644 ${imageArchive}/${imageTarball}.sha256 "$matrix/${arch}/${imageTarball}.sha256"

    mkdir -p "$root/usr/share/doc/${packageName}"
    echo "Bundled Kubernetes ${version} node stack (NKE)." \
      >"$root/usr/share/doc/${packageName}/README.Debian"

    sed \
      -e 's|{{PACKAGE_NAME}}|${packageName}|g' \
      -e 's|{{K8S_VER}}|${version}|g' \
      -e 's|{{K8S_PATCH}}|${patch}|g' \
      -e 's|{{ARCH}}|${arch}|g' \
      -e 's|{{MATRIX_SUBDIR}}|${matrixSubdir}|g' \
      ${postinstTemplate} >postinst
    chmod 0755 postinst

    mkdir -p $out
    fpm -s dir -t deb \
      -n ${packageName} \
      -v ${debRevision} \
      -a ${arch} \
      -C "$root" \
      -p "$out/${debFileName}" \
      --maintainer 'NKE <nke-dev@exchange.nvidia.com>' \
      --vendor NVIDIA \
      --license Apache-2.0 \
      --url https://github.com/NVIDIA/nke \
      --category admin \
      --deb-priority optional \
      --description ${lib.escapeShellArg description} \
      --provides ${packagePrefix} \
      ${
        lib.concatMapStringsSep " " (c: "--conflicts ${c} --replaces ${c}") (
          upstreamConflicts ++ peerConflicts
        )
      } \
      --depends iptables \
      --depends iproute2 \
      --depends conntrack \
      --depends socat \
      --depends ethtool \
      --config-files /etc/default/kubelet \
      --deb-no-default-config-files \
      --after-install postinst \
      .
  ''
