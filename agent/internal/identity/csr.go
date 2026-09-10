// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net/url"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// KeyedCSR is a certificate request and the private key that signed it. They
// travel together so a caller cannot install a certificate against a key it
// does not hold.
type KeyedCSR struct {
	// DER is the request in the form the issuer expects.
	DER []byte
	// PrivateKey never leaves this process. Only the public half is conveyed.
	PrivateKey crypto.Signer
}

// NewCSR generates a fresh keypair and a certificate request naming id as its
// only URI SAN.
//
// A new key on every call is deliberate: renewal is the point at which a key
// compromised since the last issuance stops being useful, and the cost of
// generating a P-256 key is irrelevant next to the round trip.
//
// The request carries nothing beyond that one SAN. An issuer is expected to
// refuse a subject, DNS SANs, or extra URI SANs that a requester has no
// business setting, and sending none of them keeps the agent on the safe side
// of that check rather than relying on the issuer to strip them.
func NewCSR(id spiffeid.ID) (*KeyedCSR, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("a SPIFFE ID is required")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating a private key: %w", err)
	}

	// x509.CreateCertificateRequest signs with key, so the result carries proof
	// that the requester holds the private half of the key it is presenting.
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:            pkix.Name{},
		URIs:               []*url.URL{id.URL()},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, key)
	if err != nil {
		return nil, fmt.Errorf("creating a certificate request for %q: %w", id, err)
	}

	return &KeyedCSR{DER: der, PrivateKey: key}, nil
}
