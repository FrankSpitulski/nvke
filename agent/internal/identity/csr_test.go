// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/ecdsa"
	"crypto/x509"
	"testing"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

func TestNewCSRNamesExactlyOneSPIFFEID(t *testing.T) {
	t.Parallel()

	id := spiffeid.RequireFromString("spiffe://example.org/host/node-1")

	keyed, err := NewCSR(id)
	if err != nil {
		t.Fatalf("NewCSR() = %v, want nil", err)
	}

	csr, err := x509.ParseCertificateRequest(keyed.DER)
	if err != nil {
		t.Fatalf("parsing the generated CSR: %v", err)
	}

	if len(csr.URIs) != 1 {
		t.Fatalf("CSR has %d URI SANs, want exactly 1: %v", len(csr.URIs), csr.URIs)
	}
	if got := csr.URIs[0].String(); got != id.String() {
		t.Errorf("CSR URI SAN = %q, want %q", got, id.String())
	}
}

// An issuer derives the permitted identity from the verified token and refuses
// anything else the request names. Sending none of it keeps the agent clear of
// that check rather than depending on the issuer to strip fields.
func TestNewCSRCarriesNothingElse(t *testing.T) {
	t.Parallel()

	keyed, err := NewCSR(spiffeid.RequireFromString("spiffe://example.org/host/node-1"))
	if err != nil {
		t.Fatalf("NewCSR() = %v, want nil", err)
	}
	csr, err := x509.ParseCertificateRequest(keyed.DER)
	if err != nil {
		t.Fatalf("parsing the generated CSR: %v", err)
	}

	if len(csr.DNSNames) != 0 {
		t.Errorf("CSR has DNS SANs %v, want none", csr.DNSNames)
	}
	if len(csr.IPAddresses) != 0 {
		t.Errorf("CSR has IP SANs %v, want none", csr.IPAddresses)
	}
	if len(csr.EmailAddresses) != 0 {
		t.Errorf("CSR has email SANs %v, want none", csr.EmailAddresses)
	}
	if csr.Subject.CommonName != "" {
		t.Errorf("CSR subject common name = %q, want empty", csr.Subject.CommonName)
	}
	if len(csr.Subject.Organization) != 0 {
		t.Errorf("CSR subject organization = %v, want empty", csr.Subject.Organization)
	}
}

// Proof of possession is the reason the issuer can bind this identity to this
// key. If the signature did not verify, a valid token could have a certificate
// minted against a public key its holder does not control.
func TestNewCSRProvesPossessionOfItsKey(t *testing.T) {
	t.Parallel()

	keyed, err := NewCSR(spiffeid.RequireFromString("spiffe://example.org/host/node-1"))
	if err != nil {
		t.Fatalf("NewCSR() = %v, want nil", err)
	}
	csr, err := x509.ParseCertificateRequest(keyed.DER)
	if err != nil {
		t.Fatalf("parsing the generated CSR: %v", err)
	}

	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CheckSignature() = %v, want nil", err)
	}

	public, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("CSR public key is %T, want *ecdsa.PublicKey", csr.PublicKey)
	}
	if !keyed.PrivateKey.Public().(*ecdsa.PublicKey).Equal(public) {
		t.Error("the returned private key does not match the key in the CSR")
	}
}

// Renewal is where a key compromised since the last issuance stops being
// useful, so a fresh request must not reuse the previous key.
func TestNewCSRGeneratesAFreshKeyEachTime(t *testing.T) {
	t.Parallel()

	id := spiffeid.RequireFromString("spiffe://example.org/host/node-1")

	first, err := NewCSR(id)
	if err != nil {
		t.Fatalf("NewCSR() = %v, want nil", err)
	}
	second, err := NewCSR(id)
	if err != nil {
		t.Fatalf("NewCSR() = %v, want nil", err)
	}

	firstKey := first.PrivateKey.Public().(*ecdsa.PublicKey)
	secondKey := second.PrivateKey.Public().(*ecdsa.PublicKey)
	if firstKey.Equal(secondKey) {
		t.Error("two calls produced the same key, want a fresh key per request")
	}
}

func TestNewCSRRejectsAZeroID(t *testing.T) {
	t.Parallel()

	if _, err := NewCSR(spiffeid.ID{}); err == nil {
		t.Fatal("NewCSR(zero ID) = nil, want an error")
	}
}
