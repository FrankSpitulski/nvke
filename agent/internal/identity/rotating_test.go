// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// testCA issues SVIDs for one trust domain. Renewal reissues a leaf from the
// same authority, so tests that rotate share a CA the way production does.
type testCA struct {
	trustDomain spiffeid.TrustDomain
	cert        *x509.Certificate
	key         *ecdsa.PrivateKey
	serial      atomic.Int64
}

func newTestCA(t *testing.T, trustDomain string) *testCA {
	t.Helper()

	td := spiffeid.RequireTrustDomainFromString(trustDomain)
	now := time.Now()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: trustDomain + " test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the CA certificate: %v", err)
	}

	return &testCA{trustDomain: td, cert: cert, key: key}
}

func (ca *testCA) issue(t *testing.T, path string) *x509svid.SVID {
	t.Helper()

	id := spiffeid.RequireFromPath(ca.trustDomain, path)
	now := time.Now()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a leaf key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(ca.serial.Add(1) + 1),
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{id.URL()},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("creating a leaf certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the leaf certificate: %v", err)
	}

	return &x509svid.SVID{ID: id, Certificates: []*x509.Certificate{cert}, PrivateKey: key}
}

func (ca *testCA) bundles() *x509bundle.Set {
	return x509bundle.NewSet(x509bundle.FromX509Authorities(ca.trustDomain, []*x509.Certificate{ca.cert}))
}

// verifyPair reports whether the SVID chains to an authority in the bundle,
// which is what a peer does with the material this source serves.
func verifyPair(svid *x509svid.SVID, bundle *x509bundle.Bundle) error {
	roots := x509.NewCertPool()
	for _, authority := range bundle.X509Authorities() {
		roots.AddCert(authority)
	}
	_, err := svid.Certificates[0].Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	return err
}

// A handshake attempted before the agent holds an identity has to fail cleanly
// rather than return a nil SVID for the TLS stack to dereference.
func TestRotatingSourceEmptyReportsNoIdentity(t *testing.T) {
	t.Parallel()

	var source RotatingSource

	if _, err := source.GetX509SVID(); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("GetX509SVID() error = %v, want ErrNoIdentity", err)
	}
	td := spiffeid.RequireTrustDomainFromString("example.org")
	if _, err := source.GetX509BundleForTrustDomain(td); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("GetX509BundleForTrustDomain() error = %v, want ErrNoIdentity", err)
	}
}

func TestRotatingSourceServesInstalledMaterial(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	svid := ca.issue(t, "/host/node-1")

	var source RotatingSource
	if err := source.Set(svid, ca.bundles()); err != nil {
		t.Fatalf("Set() = %v, want nil", err)
	}

	got, err := source.GetX509SVID()
	if err != nil {
		t.Fatalf("GetX509SVID() = %v, want nil", err)
	}
	if got.ID != svid.ID {
		t.Errorf("GetX509SVID().ID = %q, want %q", got.ID, svid.ID)
	}

	bundle, err := source.GetX509BundleForTrustDomain(ca.trustDomain)
	if err != nil {
		t.Fatalf("GetX509BundleForTrustDomain() = %v, want nil", err)
	}
	if err := verifyPair(got, bundle); err != nil {
		t.Errorf("the served SVID does not verify against the served bundle: %v", err)
	}
}

// The bundle decides whether a peer is trusted, so an unknown trust domain has
// to be refused rather than answered with whatever bundle happens to be held.
func TestRotatingSourceRefusesUnknownTrustDomain(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	var source RotatingSource
	if err := source.Set(ca.issue(t, "/host/node-1"), ca.bundles()); err != nil {
		t.Fatalf("Set() = %v, want nil", err)
	}

	other := spiffeid.RequireTrustDomainFromString("attacker.test")
	if _, err := source.GetX509BundleForTrustDomain(other); err == nil {
		t.Fatal("GetX509BundleForTrustDomain(attacker.test) = nil, want an error")
	}
}

// A refresh that came back unusable must leave the working identity serving,
// because the alternative is a healthy node going dark on a bad response.
func TestRotatingSourceRejectsIncompleteMaterial(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	svid := ca.issue(t, "/host/node-1")
	bundles := ca.bundles()

	noKey := *svid
	noKey.PrivateKey = nil
	noCerts := *svid
	noCerts.Certificates = nil

	tests := []struct {
		name    string
		svid    *x509svid.SVID
		bundles *x509bundle.Set
	}{
		{name: "no svid", svid: nil, bundles: bundles},
		{name: "no bundles", svid: svid, bundles: nil},
		{name: "svid without a private key", svid: &noKey, bundles: bundles},
		{name: "svid without certificates", svid: &noCerts, bundles: bundles},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var source RotatingSource
			if err := source.Set(svid, bundles); err != nil {
				t.Fatalf("installing the initial identity: %v", err)
			}

			if err := source.Set(tc.svid, tc.bundles); err == nil {
				t.Fatal("Set() = nil, want an error")
			}

			got, err := source.GetX509SVID()
			if err != nil {
				t.Fatalf("GetX509SVID() after a refused Set = %v, want the previous identity", err)
			}
			if got.ID != svid.ID {
				t.Errorf("GetX509SVID().ID = %q, want the previous %q", got.ID, svid.ID)
			}
		})
	}
}

// Renewal reissues the leaf from the same authority while handshakes are in
// flight. Readers must never see a half-installed identity: no missing key, no
// leaf that fails against the bundle served beside it. Under -race this also
// covers concurrent access to the source itself.
func TestRotatingSourceRenewalIsInvisibleToReaders(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	bundles := ca.bundles()
	leaves := []*x509svid.SVID{
		ca.issue(t, "/host/node-1"),
		ca.issue(t, "/host/node-1"),
		ca.issue(t, "/host/node-1"),
	}

	var source RotatingSource
	if err := source.Set(leaves[0], bundles); err != nil {
		t.Fatalf("installing the initial identity: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	failed := make(chan error, 1)
	report := func(err error) {
		select {
		case failed <- err:
		default:
		}
	}

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}

				svid, err := source.GetX509SVID()
				if err != nil {
					report(err)
					return
				}
				if svid.PrivateKey == nil || len(svid.Certificates) == 0 {
					report(errors.New("observed an SVID missing its key or certificates"))
					return
				}
				bundle, err := source.GetX509BundleForTrustDomain(ca.trustDomain)
				if err != nil {
					report(err)
					return
				}
				if err := verifyPair(svid, bundle); err != nil {
					report(err)
					return
				}
			}
		}()
	}

	for i := range 300 {
		if err := source.Set(leaves[i%len(leaves)], bundles); err != nil {
			t.Fatalf("rotating the identity: %v", err)
		}
	}

	close(stop)
	wg.Wait()

	select {
	case err := <-failed:
		t.Fatalf("a reader observed unusable material during renewal: %v", err)
	default:
	}
}
