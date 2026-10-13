// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checks

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// Inputs carries the facts the static tier runs on. Inspection tiers
// (guestfs, boot) add their own fact sources as they land.
type Inputs struct {
	// ImageSHA256 is the computed bare-hex digest of the image file.
	ImageSHA256 string
	// Checksums maps artifact name to its bare-hex sha256, as parsed from the
	// release's attested checksums.txt.
	Checksums map[string]string
	// Evidence reports whether (and how) the release evidence was verified
	// before the engine ran.
	Evidence EvidenceStatus
}

// EvidenceStatus is the outcome of verifying the release evidence bundle.
type EvidenceStatus struct {
	// Checked is false when the run never attempted verification (digest-only
	// --checksums mode); INT-002 then reports a skip, not a pass.
	Checked bool
	// Err is the verification failure, nil on success.
	Err error
	// SignerSAN is the verified signer identity on success.
	SignerSAN string
}

// Outcome is what a check implementation reports; the engine folds in the
// registry's ID and severity.
type Outcome struct {
	Status   Status
	Evidence string
}

// Func is one check implementation. It never prints and never exits; it
// reports an Outcome or an error (which the engine records as StatusError).
type Func func(ctx context.Context, t Target, in Inputs) (Outcome, error)

// Engine binds check IDs to implementations and runs a registry in order.
type Engine struct {
	impls map[string]Func
}

// NewEngine builds an engine over the given implementations.
func NewEngine(impls map[string]Func) *Engine {
	return &Engine{impls: impls}
}

// Run evaluates every check in registry order against one target. A check
// whose appliesTo excludes the target is reported as StatusSkip; a check with
// no implementation is StatusError — a registry/engine version drift is a
// defect, not a silent pass.
func (e *Engine) Run(ctx context.Context, reg *Registry, t Target, in Inputs) []Result {
	results := make([]Result, 0, len(reg.Checks))
	for _, def := range reg.Checks {
		res := Result{ID: def.ID, Severity: def.Severity}
		switch impl, ok := e.impls[def.ID]; {
		case !def.AppliesTo.Matches(t):
			res.Status = StatusSkip
			res.Evidence = "target outside appliesTo"
		case !ok:
			res.Status = StatusError
			res.Evidence = fmt.Sprintf("no implementation bound for %s", def.ID)
		default:
			out, err := impl(ctx, t, in)
			if err != nil {
				res.Status = StatusError
				res.Evidence = err.Error()
			} else {
				res.Status = out.Status
				res.Evidence = out.Evidence
			}
		}
		results = append(results, res)
	}
	return results
}

// newBytesReader exists so registry.go needs no direct bytes import juggling.
func newBytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
