// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package attest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// WorkloadAPIAttestor fetches a JWT-SVID from the local SPIRE agent Workload
// API. Node attestation (DPU vs TPM) is SPIRE agent plugin config, not this
// type: nke-agent only consumes the socket after the agent has an identity.
type WorkloadAPIAttestor struct {
	// SocketPath is a Workload API address (unix:///path). Empty uses
	// SPIFFE_ENDPOINT_SOCKET.
	SocketPath string
	// Audience is the JWT aud claim required by FetchJWTSVID.
	Audience string
}

// JWT returns a compact JWT-SVID for Audience.
func (w WorkloadAPIAttestor) JWT(ctx context.Context) (string, error) {
	audience := strings.TrimSpace(w.Audience)
	if audience == "" {
		return "", errors.New("jwt audience is empty")
	}

	var opts []workloadapi.JWTSourceOption
	if socket := strings.TrimSpace(w.SocketPath); socket != "" {
		opts = append(opts, workloadapi.WithClientOptions(workloadapi.WithAddr(socket)))
	}

	source, err := workloadapi.NewJWTSource(ctx, opts...)
	if err != nil {
		return "", fmt.Errorf("spire workload api jwt source: %w", err)
	}
	defer func() { _ = source.Close() }()

	svid, err := source.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience})
	if err != nil {
		return "", fmt.Errorf("fetch jwt-svid: %w", err)
	}
	if svid == nil {
		return "", errors.New("fetch jwt-svid: empty svid")
	}
	token := strings.TrimSpace(svid.Marshal())
	if token == "" {
		return "", errors.New("fetch jwt-svid: empty token")
	}
	return token, nil
}
