// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetry(t *testing.T) {
	// The error libvirt returns when two TPM domains start at once.
	swtpmRace := errors.New("failed to create directory '/run/swtpm': File exists")

	tests := []struct {
		name      string
		failures  int
		cancelled bool
		wantCalls int
		wantErr   error
	}{
		{name: "succeeds first time", failures: 0, wantCalls: 1},
		{name: "second attempt wins the race", failures: 1, wantCalls: 2},
		{name: "every attempt fails", failures: 3, wantCalls: 3, wantErr: swtpmRace},
		{name: "cancelled context stops retrying", failures: 3, cancelled: true, wantCalls: 1, wantErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}

			calls := 0
			err := retry(ctx, 3, time.Millisecond, func() error {
				calls++
				if calls <= tt.failures {
					return swtpmRace
				}
				return nil
			})

			if calls != tt.wantCalls {
				t.Errorf("called fn %d times, want %d", calls, tt.wantCalls)
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("retry returned %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("retry returned %v, want %v", err, tt.wantErr)
			}
		})
	}
}
