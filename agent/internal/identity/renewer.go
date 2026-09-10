// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// Defaults for Renewer.
const (
	// DefaultRenewFraction renews half way through the certificate's life,
	// leaving the whole second half to retry a controller that is unreachable.
	DefaultRenewFraction   = 0.5
	DefaultRenewMinBackoff = 5 * time.Second
	DefaultRenewMaxBackoff = 2 * time.Minute
)

// TokenSource supplies the attestation token the controller verifies. The
// token is short lived and fetched per exchange rather than cached, so a
// renewal always presents a currently valid assertion.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Issued is the material the controller returns for an accepted request.
type Issued struct {
	// Certificates is the issued chain, leaf first.
	Certificates []*x509.Certificate
	// Bundle verifies peers, and is the issuer's bundle rather than the
	// agent's own, so the listener can authenticate the controller.
	Bundle *x509bundle.Set
}

// Attestor exchanges a token and a certificate request for a minted SVID.
// The transport lives behind this so the exchange can be exercised without one.
type Attestor interface {
	Attest(ctx context.Context, token string, csrDER []byte) (*Issued, error)
}

// Renewer keeps a RotatingSource stocked with an unexpired identity.
//
// Renewal is the same exchange as first issuance, deliberately: one endpoint,
// one set of invariants, and every renewal re-presents a fresh token so the
// assertion that admitted this host is re-verified rather than trusted
// indefinitely from enrollment.
//
// The loop is a backstop, not the primary schedule. A controller that pushes
// rotation over the established channel is free to do so, but a certificate
// that expires takes the channel with it, so the agent has to be able to
// recover without being reached first.
type Renewer struct {
	// ID is the identity requested. The issued certificate is refused if it
	// names anything else.
	ID spiffeid.ID

	Tokens   TokenSource
	Attestor Attestor
	Source   *RotatingSource

	// RenewFraction is how far through the certificate's life to renew.
	RenewFraction float64
	// MinBackoff and MaxBackoff bound the delay between failed attempts.
	MinBackoff time.Duration
	MaxBackoff time.Duration

	// Observe, when set, is called once per finished attempt.
	Observe func(Attempt)
}

// Renew performs one exchange and installs the result.
//
// A failure returns without touching the source, so a renewal that could not
// complete leaves a still-valid certificate serving rather than taking down a
// working listener.
func (r *Renewer) Renew(ctx context.Context) error {
	if r.Source == nil {
		return errors.New("a source is required")
	}

	token, err := r.Tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("obtaining an attestation token: %w", err)
	}

	keyed, err := NewCSR(r.ID)
	if err != nil {
		return err
	}

	issued, err := r.Attestor.Attest(ctx, token, keyed.DER)
	if err != nil {
		return fmt.Errorf("exchanging the attestation token: %w", err)
	}
	if issued == nil || len(issued.Certificates) == 0 {
		return errors.New("the issuer returned no certificates")
	}
	if issued.Bundle == nil {
		return errors.New("the issuer returned no trust bundle")
	}

	// The issuer derives the identity from the verified token, so a mismatch
	// means we would serve an identity we did not ask for. Refuse it here too
	// rather than discovering it when a peer rejects the handshake.
	leaf := issued.Certificates[0]
	if len(leaf.URIs) != 1 {
		return fmt.Errorf("issued certificate has %d URI SANs, want exactly 1", len(leaf.URIs))
	}
	issuedID, err := spiffeid.FromURI(leaf.URIs[0])
	if err != nil {
		return fmt.Errorf("parsing the issued SPIFFE ID: %w", err)
	}
	if issuedID != r.ID {
		return fmt.Errorf("issued certificate names %q, want %q", issuedID, r.ID)
	}

	return r.Source.Set(&x509svid.SVID{
		ID:           issuedID,
		Certificates: issued.Certificates,
		PrivateKey:   keyed.PrivateKey,
	}, issued.Bundle)
}

// Run acquires an identity and keeps it current until ctx is done.
//
// It blocks until the first exchange succeeds, retrying indefinitely, because
// a host that cannot reach the controller yet may reach it later and exiting
// would only lose the diagnostics. After that it renews part way through each
// certificate's life, or as soon as trigger fires, whichever comes first.
// A nil trigger disables the push path and leaves only the timer.
func (r *Renewer) Run(ctx context.Context, trigger <-chan struct{}) error {
	minBackoff := orDefault(r.MinBackoff, DefaultRenewMinBackoff)
	maxBackoff := orDefault(r.MaxBackoff, DefaultRenewMaxBackoff)
	if maxBackoff < minBackoff {
		maxBackoff = minBackoff
	}

	backoff := minBackoff
	for n := 1; ; n++ {
		err := r.Renew(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("renewing the agent identity: %w", ctxErr)
		}

		var wait time.Duration
		switch {
		case err != nil:
			wait = backoff
			backoff = min(backoff*2, maxBackoff)
		default:
			backoff = minBackoff
			wait = r.untilRenewal()
		}

		if r.Observe != nil {
			r.Observe(Attempt{N: n, Err: err, RetryIn: wait})
		}
		if err != nil {
			slog.Warn("renewing the agent identity failed, keeping the current one",
				"attempt", n, "retryIn", wait, "err", err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("renewing the agent identity: %w", ctx.Err())
		case <-trigger:
			// A push arrived. Renew now regardless of where the timer stood.
		case <-after(wait):
		}
	}
}

// untilRenewal returns how long to wait before renewing the installed
// certificate. It is zero when the certificate is already past that point,
// which makes an expired or nearly expired identity retry immediately.
func (r *Renewer) untilRenewal() time.Duration {
	svid, err := r.Source.GetX509SVID()
	if err != nil || len(svid.Certificates) == 0 {
		return 0
	}

	fraction := r.RenewFraction
	if fraction <= 0 || fraction >= 1 {
		fraction = DefaultRenewFraction
	}

	leaf := svid.Certificates[0]
	lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
	if lifetime <= 0 {
		return 0
	}

	renewAt := leaf.NotBefore.Add(time.Duration(float64(lifetime) * fraction))
	return max(time.Until(renewAt), 0)
}

// after returns a channel that fires after d, or never when d is not positive
// and the caller should proceed immediately on another case.
func after(d time.Duration) <-chan time.Time {
	if d <= 0 {
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	return time.After(d)
}
