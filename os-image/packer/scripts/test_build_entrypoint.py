#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Exercise Make's Packer argv and early failures without downloading or booting."""
import json
import os
import shutil
from pathlib import Path
import subprocess
import tempfile
import unittest

PACKER_DIR = Path(__file__).resolve().parents[1]


class BuildEntrypointTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.packer_dir = self.root / "packer-project"
        self.packer_dir.mkdir()
        shutil.copy(PACKER_DIR / "Makefile", self.packer_dir)
        shutil.copytree(PACKER_DIR / "scripts", self.packer_dir / "scripts")
        self.env = os.environ.copy()
        self.env.update(
            PATH=f"{self.root}:{self.env['PATH']}",
            OVMF_CODE=str(self.root / "code.fd"),
            OVMF_VARS_TEMPLATE=str(self.root / "vars.fd"),
            PACKER_ARGS=str(self.root / "args.json"),
        )
        for name in ("code.fd", "vars.fd"):
            (self.root / name).touch()
        self.script("uname", '#!/bin/sh\ncase "$1" in -m) echo aarch64;; -s) echo Linux;; esac\n')
        # nke_qemu_env resolves the QEMU binary by name off PATH; check-deps
        # looks for qemu-img, which the macOS CI runner lacks.
        self.script("qemu-system-aarch64", '#!/bin/sh\nif [ "$1" = -accel ]; then echo tcg; fi\n')
        self.script("qemu-img", '#!/bin/sh\nexit 0\n')
        self.script("packer", '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
Path(os.environ['PACKER_ARGS']).write_text(json.dumps(sys.argv[1:]))
values = dict(arg.split('=', 1) for arg in sys.argv[1:] if '=' in arg)
variant = 'non-fde' if 'non-fde' in values['-only'] else 'fde'
output = Path(values['output_directory']) / variant
output.mkdir(parents=True, exist_ok=True)
(output / 'nke-node.qcow2').touch()
''')

    def script(self, name, text):
        p = self.root / name
        p.write_text(text)
        p.chmod(0o755)

    def make(self, target, *extra, skip_build_setup=False):
        args = [os.environ.get("MAKE", "make"), "--no-print-directory", "-C", str(self.packer_dir)]
        if skip_build_setup:
            args += ["-o", "prepare-image-build"]
            target = "_" + target
        args += [target, "ARCH=arm64", "ACCELERATOR=tcg", f"PACKER={self.root / 'packer'}",
                 f"ARTIFACTS_DIR={self.root / 'artifacts'}", f"BUILD_LOGS={self.root / 'logs'}", *extra]
        return subprocess.run(args, env=self.env, text=True, capture_output=True, check=False)

    def test_both_variants_pass_firmware_before_template_and_keep_current_defaults(self):
        for variant in ("fde", "non-fde"):
            with self.subTest(variant=variant):
                result = self.make(f"os-image-{variant}", skip_build_setup=True)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                args = json.loads(Path(self.env['PACKER_ARGS']).read_text())
                self.assertEqual(args[0], "build")
                self.assertEqual(args[-1], str(self.packer_dir))
                for value in ("accelerator=tcg", "cpu_model=max,pauth-impdef=on", "ssh_timeout=180m",
                              "root_fs_label=cloudimg-rootfs", f"material_manifest={self.packer_dir / '.packer-inputs/material-manifest.json'}",
                              f"efi_firmware_code={self.env['OVMF_CODE']}",
                              f"efi_firmware_vars={self.env['OVMF_VARS_TEMPLATE']}"):
                    self.assertIn(value, args)

    def test_native_requirement_rejects_tcg_before_packer_build(self):
        result = self.make("os-image-non-fde", "REQUIRE_ACCELERATION=1", skip_build_setup=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot be hardware accelerated", result.stderr)
        self.assertFalse(Path(self.env['PACKER_ARGS']).exists())

    def test_acceleration_preflight_rejects_hvf_and_mismatched_iso_arch(self):
        cases = (
            (["ACCELERATOR=hvf"], "not supported for image builds"),
            (["ISO_URL=https://example.invalid/ubuntu-26.04.1-live-server-amd64.iso"],
             "does not match ARCH=arm64"),
        )
        for extra, expected in cases:
            with self.subTest(extra=extra):
                result = self.make("check-acceleration", *extra)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)

    def test_package_preflight_rejects_macos_and_mismatched_linux(self):
        for system, arch in (("Darwin", "arm64"), ("Linux", "x86_64")):
            with self.subTest(system=system, arch=arch):
                self.script("uname", f'#!/bin/sh\ncase "$1" in -m) echo {arch};; -s) echo {system};; esac\n')
                result = self.make("check-deps")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("matching-architecture Linux host", result.stderr)


if __name__ == "__main__":
    unittest.main()
