// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package runner drives a full validation run from resolved options: evidence
// loading, inspection-tier setup, engine execution, reporting and verdict
// emission. It is the shared engine behind cmd/nvke-image-validate and
// `nvkectl validate image`; flag parsing stays with each CLI.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/validate/pkg/bundle"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/boot"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/imagefs"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/static"
	"github.com/dsx-ai-factory/nvke/validate/pkg/inspect/guestfs"
	"github.com/dsx-ai-factory/nvke/validate/pkg/inspect/qemu"
	"github.com/dsx-ai-factory/nvke/validate/pkg/report"
	"github.com/dsx-ai-factory/nvke/validate/pkg/verify"
)

// Evidence-signer identity defaults: the release workflow that attests
// NVIDIA-built node-image evidence.
const (
	DefaultCertIssuer   = "https://token.actions.githubusercontent.com"
	DefaultCertSANRegex = `^https://github\.com/dsx-ai-factory/nvke/\.github/workflows/os-image-artifact\.yml@.+$`
)

// Options is one validation run, fully resolved. Zero values take the
// documented defaults where one exists; the required fields are ImagePath,
// RegistryPath, Variant, Arch, Ubuntu and exactly one of BundleDir or
// ChecksumsPath.
type Options struct {
	// ImagePath is the qcow2 under validation.
	ImagePath string
	// Name is the artifact's release name; empty means ImagePath's basename.
	Name string
	// BundleDir is the release evidence bundle directory; verified before any
	// check runs. Exactly one of BundleDir and ChecksumsPath must be set.
	BundleDir string
	// ChecksumsPath is a bare checksums.txt: digest-only mode, with the
	// unverified evidence reported as a visible skip.
	ChecksumsPath string
	// CertIssuer and CertSANRegex pin the evidence signer's identity in bundle
	// mode; empty means the NVIDIA release workflow defaults above.
	CertIssuer   string
	CertSANRegex string
	// RegistryPath is the checks.yaml to run.
	RegistryPath string
	// Variant, Arch and Ubuntu are the validation target.
	Variant string
	Arch    string
	Ubuntu  string
	// BOMPath names the component versions the target expects; empty means the
	// BOM-driven COMPAT checks report a skip.
	BOMPath string
	// Inspect controls the guestfs tier: auto (on when guestfish is on PATH),
	// on, or off. Empty means auto.
	Inspect string
	// Boot controls the boot tier (boots a throwaway copy under QEMU): on or
	// off. Empty means off.
	Boot string
	// LUKSPassphraseFile holds the passphrase for opening FDE images.
	LUKSPassphraseFile string
	// EmitVerdictPath writes the run's result as a DSSE-signed attestation;
	// it requires SignKeyPath (a PEM Ed25519 or ECDSA P-256 private key).
	EmitVerdictPath string
	SignKeyPath     string
	// Format is the report rendering: text or json. Empty means text.
	Format string
	// ValidatorVersion is stamped into emitted verdicts.
	ValidatorVersion string
}

// Run executes one validation run, writing the report to stdout: exit 0 pass,
// 1 blocking fail, 2 could-not-evaluate. At most one document reaches stdout —
// a non-nil error means nothing was written, and a post-report failure rides
// the report as runError.
func Run(ctx context.Context, opts Options, stdout io.Writer) (int, error) {
	for field, v := range map[string]string{
		"image": opts.ImagePath, "checks registry": opts.RegistryPath,
		"target variant": opts.Variant, "target arch": opts.Arch, "target ubuntu": opts.Ubuntu,
	} {
		if v == "" {
			return report.ExitError, fmt.Errorf("the %s is required", field)
		}
	}
	if (opts.BundleDir == "") == (opts.ChecksumsPath == "") {
		return report.ExitError, fmt.Errorf("exactly one of the evidence bundle and the checksums file is required")
	}
	if (opts.EmitVerdictPath == "") != (opts.SignKeyPath == "") {
		return report.ExitError, fmt.Errorf("verdict emission needs both the output path and the signing key")
	}
	name := opts.Name
	if name == "" {
		name = filepath.Base(opts.ImagePath)
	}
	certIssuer := opts.CertIssuer
	if certIssuer == "" {
		certIssuer = DefaultCertIssuer
	}
	certSANRegex := opts.CertSANRegex
	if certSANRegex == "" {
		certSANRegex = DefaultCertSANRegex
	}

	// One read per policy input: the bytes hashed into the verdict must be
	// the bytes that were parsed and ran.
	registryRaw, err := os.ReadFile(opts.RegistryPath)
	if err != nil {
		return report.ExitError, fmt.Errorf("reading check registry: %w", err)
	}
	reg, err := checks.ParseRegistry(registryRaw)
	if err != nil {
		return report.ExitError, fmt.Errorf("check registry %s: %w", opts.RegistryPath, err)
	}
	checkSetSHA256 := sha256Hex(registryRaw)
	var sums map[string]string
	var evidence checks.EvidenceStatus
	var evidenceBundle *bundle.Bundle
	if opts.BundleDir != "" {
		b, err := bundle.Load(opts.BundleDir)
		if err != nil {
			return report.ExitError, err
		}
		sums = b.Checksums
		evidenceBundle = b
		evidence.Checked = true
		ev, err := b.VerifyEvidence(bundle.Identity{Issuer: certIssuer, SANRegex: certSANRegex})
		if err != nil {
			evidence.Err = err
		} else {
			evidence.SignerSAN = ev.SignerSAN
		}
	} else {
		sums, err = static.ParseChecksums(opts.ChecksumsPath)
		if err != nil {
			return report.ExitError, err
		}
	}
	digest, err := static.HashFile(opts.ImagePath)
	if err != nil {
		return report.ExitError, err
	}
	var bom map[string]string
	bomSHA256 := ""
	if opts.BOMPath != "" {
		bomRaw, err := os.ReadFile(opts.BOMPath)
		if err != nil {
			return report.ExitError, fmt.Errorf("reading target BOM: %w", err)
		}
		if bom, err = static.ParseBOMBytes(bomRaw); err != nil {
			return report.ExitError, fmt.Errorf("target BOM %s: %w", opts.BOMPath, err)
		}
		bomSHA256 = sha256Hex(bomRaw)
	}

	var imgFS checks.ImageFS
	var sbom map[string]string
	runInspection := false
	switch opts.Inspect {
	case "on":
		runInspection = true
	case "auto", "":
		runInspection = guestfs.Available()
	case "off":
	default:
		return report.ExitError, fmt.Errorf("unknown inspect mode %q (want auto, on or off)", opts.Inspect)
	}
	if runInspection {
		var passphrase string
		if opts.LUKSPassphraseFile != "" {
			raw, err := os.ReadFile(opts.LUKSPassphraseFile)
			if err != nil {
				return report.ExitError, err
			}
			passphrase = strings.TrimRight(string(raw), "\n")
		}
		session, err := guestfs.Open(ctx, opts.ImagePath, guestfs.Options{LUKSPassphrase: passphrase})
		if err != nil {
			return report.ExitError, fmt.Errorf("opening image for inspection: %w", err)
		}
		defer session.Close()
		imgFS = session
	}
	// The SBOM backs INT-004 (inspection) and COMPAT-002 (target BOM), so it
	// loads whenever either consumer runs; in digest-only mode COMPAT-002
	// reports the missing SBOM itself.
	if evidenceBundle != nil && (runInspection || bom != nil) {
		sbom, err = evidenceBundle.LoadSBOM(name)
		if err != nil {
			return report.ExitError, err
		}
	}

	var prober checks.Prober
	switch opts.Boot {
	case "off", "":
	case "on":
		if !qemu.Available(opts.Arch) {
			return report.ExitError, fmt.Errorf("the boot tier needs qemu-system for %s plus cloud-localds or genisoimage", opts.Arch)
		}
		workDir, err := os.MkdirTemp("", "nvke-validate-boot-*")
		if err != nil {
			return report.ExitError, err
		}
		defer os.RemoveAll(workDir)
		keyPath, pubKey, err := qemu.GenerateSSHKey(workDir)
		if err != nil {
			return report.ExitError, err
		}
		seed, err := qemu.MakeSeed(workDir, "nvkeprobe", pubKey)
		if err != nil {
			return report.ExitError, err
		}
		vm, err := qemu.Boot(ctx, opts.ImagePath, qemu.Options{Arch: opts.Arch, SeedISO: seed, SSHKeyPath: keyPath})
		if err != nil {
			return report.ExitError, fmt.Errorf("boot tier: %w", err)
		}
		defer vm.Close()
		prober = vm
	default:
		return report.ExitError, fmt.Errorf("unknown boot mode %q (want on or off)", opts.Boot)
	}

	target := checks.Target{
		ImagePath: opts.ImagePath,
		ImageName: name,
		Variant:   checks.Variant(opts.Variant),
		Arch:      opts.Arch,
		Ubuntu:    opts.Ubuntu,
	}
	impls := imagefs.Merge(static.Impls(), imagefs.Impls(), boot.Impls())
	results := checks.NewEngine(impls).Run(ctx, reg, target, checks.Inputs{
		ImageSHA256:  digest,
		Checksums:    sums,
		Evidence:     evidence,
		FS:           imgFS,
		SBOMPackages: sbom,
		BOM:          bom,
		Boot:         prober,
	})
	rep := report.New(reg, target, digest, results, time.Now())
	if opts.EmitVerdictPath != "" {
		if err := writeVerdict(opts, rep, checkSetSHA256, bomSHA256); err != nil {
			// The signing failure rides the one report document, never a
			// second JSON object.
			rep.RunError = fmt.Sprintf("emitting verdict: %v", err)
			rep.Summary.Error++
		}
	}

	switch opts.Format {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			return report.ExitError, err
		}
		if _, err := stdout.Write(out); err != nil {
			return report.ExitError, err
		}
	case "text", "":
		if _, err := io.WriteString(stdout, rep.Text()); err != nil {
			return report.ExitError, err
		}
	default:
		return report.ExitError, fmt.Errorf("unknown report format %q (want text or json)", opts.Format)
	}
	return rep.ExitCode(), nil
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// writeVerdict signs the run's result as an in-toto attestation over the
// image's digest. A failing or erroring run still emits its verdict: the
// signed outcome is the evidence a consumer enforces on.
func writeVerdict(opts Options, rep report.Report, checkSetSHA256, bomSHA256 string) error {
	keyPEM, err := os.ReadFile(opts.SignKeyPath)
	if err != nil {
		return fmt.Errorf("reading signing key: %w", err)
	}
	signer, err := verify.LoadSigner(keyPEM)
	if err != nil {
		return err
	}
	outcome := verify.OutcomeError
	switch rep.ExitCode() {
	case report.ExitPass:
		outcome = verify.OutcomePass
	case report.ExitFail:
		outcome = verify.OutcomeFail
	}
	validatorVersion := opts.ValidatorVersion
	if validatorVersion == "" {
		validatorVersion = "unknown"
	}
	results := make([]verify.CheckResult, 0, len(rep.Results))
	for _, r := range rep.Results {
		results = append(results, verify.CheckResult{ID: r.ID, Status: string(r.Status), Severity: string(r.Severity), Reason: string(r.Reason), Evidence: r.Evidence})
	}
	evidenceMode := verify.EvidenceModeBundle
	if opts.BundleDir == "" {
		evidenceMode = verify.EvidenceModeDigestOnly
	}
	statement := verify.Statement{
		Type:          verify.StatementType,
		Subject:       []verify.Subject{{Name: rep.Subject.Name, Digest: map[string]string{"sha256": rep.Subject.SHA256}}},
		PredicateType: verify.PredicateType,
		Predicate: verify.Predicate{
			Outcome:             outcome,
			CheckSetVersion:     rep.CheckSetVersion,
			ReportSchemaVersion: rep.SchemaVersion,
			Validator:           verify.Tool{Name: "nvke-image-validate", Version: validatorVersion},
			Target:              verify.Target{Variant: string(rep.Target.Variant), Arch: rep.Target.Arch, Ubuntu: rep.Target.Ubuntu},
			Summary: verify.Summary{
				Pass: rep.Summary.Pass, Fail: rep.Summary.Fail, BlockingFail: rep.Summary.BlockingFail,
				WarnFail: rep.Summary.WarnFail, Skip: rep.Summary.Skip, Error: rep.Summary.Error,
			},
			Results:        results,
			FinishedAt:     rep.GeneratedAt,
			EvidenceMode:   evidenceMode,
			CheckSetSHA256: checkSetSHA256,
			BOMSHA256:      bomSHA256,
		},
	}
	payload, err := json.Marshal(statement)
	if err != nil {
		return fmt.Errorf("encoding verdict statement: %w", err)
	}
	envelope, err := verify.Sign(payload, signer)
	if err != nil {
		return err
	}
	if err := os.WriteFile(opts.EmitVerdictPath, envelope, 0o644); err != nil {
		return fmt.Errorf("writing verdict: %w", err)
	}
	return nil
}
