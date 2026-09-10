// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// material is the SVID and the bundles that were issued together. It is never
// mutated after being published, so readers can hold one without a lock.
type material struct {
	svid    *x509svid.SVID
	bundles *x509bundle.Set
}

// RotatingSource is a Source whose material is replaced in place, for identities
// that arrive from somewhere other than the Workload API and have to be renewed
// before they expire.
//
// It exists because tls.Config.GetCertificate rotates only the certificate,
// while the bundle is what verifies the peer, so renewing both needs a seam
// that carries the pair. A listener built with tlsconfig.MTLSServerConfig over
// this source picks up new material with no listener restart and no change to
// the authorizer.
//
// Each Set publishes an immutable pair, so no reader ever observes a
// half-installed identity. A caller still makes two separate calls to read the
// SVID and the bundle, though, and a rotation between them yields material from
// two generations. That is benign for renewal, which reissues the leaf from the
// same authority, and it stays benign across an authority roll only because the
// issuer publishes the incoming authority in the bundle before minting against
// it. This type cannot make those two reads atomic; the Source interface has no
// way to express it.
//
// The zero value is usable and empty: it reports an error until the first
// Set, which is what a handshake attempted before the agent holds an identity
// should do.
type RotatingSource struct {
	current atomic.Pointer[material]
}

var _ Source = (*RotatingSource)(nil)

// ErrNoIdentity is returned before any identity has been installed.
var ErrNoIdentity = errors.New("no X.509 identity has been issued to this agent yet")

// Set installs an SVID and the bundles issued with it, replacing any previous
// pair atomically.
//
// It rejects incomplete material rather than installing it, because the caller
// is a refresh loop: a renewal that came back unusable must leave the working
// identity in place rather than blanking out a listener that was serving.
func (s *RotatingSource) Set(svid *x509svid.SVID, bundles *x509bundle.Set) error {
	switch {
	case svid == nil:
		return errors.New("svid is required")
	case len(svid.Certificates) == 0:
		return errors.New("svid carries no certificates")
	case svid.PrivateKey == nil:
		return errors.New("svid carries no private key")
	case bundles == nil:
		return errors.New("bundles are required")
	}

	s.current.Store(&material{svid: svid, bundles: bundles})
	return nil
}

// GetX509SVID returns the current SVID, or ErrNoIdentity before the first Set.
func (s *RotatingSource) GetX509SVID() (*x509svid.SVID, error) {
	current := s.current.Load()
	if current == nil {
		return nil, ErrNoIdentity
	}
	return current.svid, nil
}

// GetX509BundleForTrustDomain returns the bundle verifying peers from
// trustDomain. An unknown trust domain is an error rather than a fallback to
// whatever bundle is held, since that decides whether a peer is trusted.
func (s *RotatingSource) GetX509BundleForTrustDomain(trustDomain spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	current := s.current.Load()
	if current == nil {
		return nil, ErrNoIdentity
	}

	bundle, err := current.bundles.GetX509BundleForTrustDomain(trustDomain)
	if err != nil {
		return nil, fmt.Errorf("no trust bundle for %q: %w", trustDomain, err)
	}
	return bundle, nil
}
