// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"math/big"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

type fixedTokens struct {
	token string
	err   error
	calls int
	mu    sync.Mutex
}

func (f *fixedTokens) Token(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.token, f.err
}

func (f *fixedTokens) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeAttestor mints from a test CA, standing in for the controller so the
// exchange can be exercised without one.
type fakeAttestor struct {
	ca       *testCA
	lifetime time.Duration

	mu        sync.Mutex
	calls     int
	tokens    []string
	failWith  error
	issueAsID *spiffeid.ID
}

func (a *fakeAttestor) Attest(_ context.Context, token string, csrDER []byte) (*Issued, error) {
	a.mu.Lock()
	a.calls++
	a.tokens = append(a.tokens, token)
	failWith, issueAsID := a.failWith, a.issueAsID
	a.mu.Unlock()

	if failWith != nil {
		return nil, failWith
	}

	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, err
	}
	// The real endpoint checks proof of possession, so the fake does too.
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}

	uri := csr.URIs[0]
	if issueAsID != nil {
		uri = issueAsID.URL()
	}

	lifetime := a.lifetime
	if lifetime == 0 {
		lifetime = time.Hour
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(a.ca.serial.Add(1) + 100),
		NotBefore:    now,
		NotAfter:     now.Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.ca.cert, csr.PublicKey, a.ca.key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	return &Issued{Certificates: []*x509.Certificate{cert}, Bundle: a.ca.bundles()}, nil
}

func (a *fakeAttestor) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *fakeAttestor) setFailure(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failWith = err
}

func (a *fakeAttestor) presentedTokens() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.tokens...)
}

func newRenewer(t *testing.T, tokens TokenSource, attestor Attestor) (*Renewer, spiffeid.ID) {
	t.Helper()
	id := spiffeid.RequireFromString("spiffe://example.org/host/node-1")
	return &Renewer{
		ID:       id,
		Tokens:   tokens,
		Attestor: attestor,
		Source:   &RotatingSource{},
	}, id
}

func TestRenewInstallsAUsableIdentity(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	renewer, id := newRenewer(t, &fixedTokens{token: "token-1"}, &fakeAttestor{ca: ca})

	if err := renewer.Renew(context.Background()); err != nil {
		t.Fatalf("Renew() = %v, want nil", err)
	}

	svid, err := renewer.Source.GetX509SVID()
	if err != nil {
		t.Fatalf("GetX509SVID() = %v, want nil", err)
	}
	if svid.ID != id {
		t.Errorf("installed ID = %q, want %q", svid.ID, id)
	}
	if svid.PrivateKey == nil {
		t.Error("installed SVID has no private key")
	}

	bundle, err := renewer.Source.GetX509BundleForTrustDomain(ca.trustDomain)
	if err != nil {
		t.Fatalf("GetX509BundleForTrustDomain() = %v, want nil", err)
	}
	if err := verifyPair(svid, bundle); err != nil {
		t.Errorf("installed SVID does not verify against the installed bundle: %v", err)
	}
}

// The issuer derives the identity from the verified token. A certificate for
// some other identity means something went wrong upstream, and serving it
// would present an identity this host never asked for.
func TestRenewRefusesACertificateForAnotherIdentity(t *testing.T) {
	t.Parallel()

	other := spiffeid.RequireFromString("spiffe://example.org/host/node-2")
	ca := newTestCA(t, "example.org")
	renewer, _ := newRenewer(t, &fixedTokens{token: "token-1"}, &fakeAttestor{ca: ca, issueAsID: &other})

	err := renewer.Renew(context.Background())
	if err == nil {
		t.Fatal("Renew() = nil, want an error")
	}
	if _, err := renewer.Source.GetX509SVID(); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("a refused certificate was installed anyway: %v", err)
	}
}

// The whole point of the backstop: a renewal that cannot complete must leave
// the working identity serving rather than blank out the listener.
func TestRenewFailureKeepsTheCurrentIdentity(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	attestor := &fakeAttestor{ca: ca}
	renewer, id := newRenewer(t, &fixedTokens{token: "token-1"}, attestor)

	if err := renewer.Renew(context.Background()); err != nil {
		t.Fatalf("the initial Renew() = %v, want nil", err)
	}
	before, err := renewer.Source.GetX509SVID()
	if err != nil {
		t.Fatalf("GetX509SVID() = %v, want nil", err)
	}

	attestor.setFailure(errors.New("controller unreachable"))
	if err := renewer.Renew(context.Background()); err == nil {
		t.Fatal("Renew() = nil, want an error")
	}

	after, err := renewer.Source.GetX509SVID()
	if err != nil {
		t.Fatalf("GetX509SVID() after a failed renewal = %v, want the previous identity", err)
	}
	if after.ID != id {
		t.Errorf("installed ID = %q, want the previous %q", after.ID, id)
	}
	if !after.Certificates[0].Equal(before.Certificates[0]) {
		t.Error("a failed renewal replaced the serving certificate")
	}
}

func TestRenewSurfacesATokenFailure(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	tokens := &fixedTokens{err: errors.New("no metadata response")}
	attestor := &fakeAttestor{ca: ca}
	renewer, _ := newRenewer(t, tokens, attestor)

	if err := renewer.Renew(context.Background()); err == nil {
		t.Fatal("Renew() = nil, want an error")
	}
	// A token that could not be obtained must not reach the controller.
	if got := attestor.count(); got != 0 {
		t.Errorf("attestor called %d times, want 0 when no token was available", got)
	}
}

// Run must not give up while the controller is unreachable, because a host
// that cannot attest yet may attest later.
func TestRunRetriesUntilTheFirstIdentityArrives(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	attestor := &fakeAttestor{ca: ca}
	attestor.setFailure(errors.New("controller unreachable"))
	renewer, _ := newRenewer(t, &fixedTokens{token: "token-1"}, attestor)
	renewer.MinBackoff = time.Millisecond
	renewer.MaxBackoff = 2 * time.Millisecond

	acquired := make(chan struct{})
	var once sync.Once
	renewer.Observe = func(attempt Attempt) {
		if attempt.Err == nil {
			once.Do(func() { close(acquired) })
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = renewer.Run(ctx, nil) }()

	// Let it fail a few times before letting it through.
	time.Sleep(20 * time.Millisecond)
	if _, err := renewer.Source.GetX509SVID(); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("an identity was installed while the controller was failing: %v", err)
	}
	attestor.setFailure(nil)

	select {
	case <-acquired:
	case <-ctx.Done():
		t.Fatal("Run never acquired an identity after the controller recovered")
	}

	if _, err := renewer.Source.GetX509SVID(); err != nil {
		t.Fatalf("GetX509SVID() after recovery = %v, want an identity", err)
	}
	if attestor.count() < 2 {
		t.Errorf("attestor called %d times, want repeated attempts", attestor.count())
	}
}

// The push path: a trigger renews immediately rather than waiting out the
// timer, which for an hour-long certificate would otherwise be half an hour.
func TestRunRenewsOnTrigger(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	tokens := &fixedTokens{token: "token-1"}
	attestor := &fakeAttestor{ca: ca, lifetime: time.Hour}
	renewer, _ := newRenewer(t, tokens, attestor)

	renewed := make(chan int, 16)
	renewer.Observe = func(attempt Attempt) {
		if attempt.Err == nil {
			select {
			case renewed <- attempt.N:
			default:
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	trigger := make(chan struct{})
	go func() { _ = renewer.Run(ctx, trigger) }()

	select {
	case <-renewed:
	case <-ctx.Done():
		t.Fatal("Run never acquired the first identity")
	}

	first, err := renewer.Source.GetX509SVID()
	if err != nil {
		t.Fatalf("GetX509SVID() = %v, want nil", err)
	}

	select {
	case trigger <- struct{}{}:
	case <-ctx.Done():
		t.Fatal("Run never became ready to accept a trigger")
	}

	select {
	case <-renewed:
	case <-ctx.Done():
		t.Fatal("a trigger did not cause a renewal")
	}

	second, err := renewer.Source.GetX509SVID()
	if err != nil {
		t.Fatalf("GetX509SVID() = %v, want nil", err)
	}
	if second.Certificates[0].Equal(first.Certificates[0]) {
		t.Error("the triggered renewal did not replace the certificate")
	}

	// Every exchange presents a freshly fetched token rather than a cached one,
	// so the assertion is re-verified on each renewal.
	if got := len(attestor.presentedTokens()); tokens.count() < got {
		t.Errorf("presented %d tokens from %d fetches, want a fetch per exchange", got, tokens.count())
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	renewer, _ := newRenewer(t, &fixedTokens{token: "token-1"}, &fakeAttestor{ca: ca})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- renewer.Run(ctx, nil) }()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() = %v, want a context cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// A certificate already past its renewal point must be retried at once rather
// than scheduled into the past and slept on.
func TestUntilRenewalIsZeroForAnExpiredCertificate(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t, "example.org")
	renewer, _ := newRenewer(t, &fixedTokens{token: "token-1"}, &fakeAttestor{ca: ca})

	// testCA issues from an hour ago to an hour ahead, so the halfway point
	// is already behind us.
	if err := renewer.Source.Set(ca.issue(t, "/host/node-1"), ca.bundles()); err != nil {
		t.Fatalf("installing an identity: %v", err)
	}

	if got := renewer.untilRenewal(); got != 0 {
		t.Errorf("untilRenewal() = %v, want 0 for a certificate past its renewal point", got)
	}
}

func TestUntilRenewalIsZeroWithNoIdentity(t *testing.T) {
	t.Parallel()

	renewer, _ := newRenewer(t, &fixedTokens{}, nil)
	if got := renewer.untilRenewal(); got != 0 {
		t.Errorf("untilRenewal() = %v, want 0 when no identity is held", got)
	}
}
