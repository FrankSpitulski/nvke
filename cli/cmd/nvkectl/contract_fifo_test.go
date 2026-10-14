// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenContractFIFO(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		timeout       time.Duration
		want          string
	}{
		{"early exit", "exit 7", 2 * time.Second, "exit"},
		{"no reader", "exec sleep 60", 100 * time.Millisecond, "deadline"},
		{"reader connects", `exec cat "$1" >/dev/null`, 2 * time.Second, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fifo := filepath.Join(t.TempDir(), "spec.yaml")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", tc.command, "fifo-reader", fifo)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
			ctx, cancel := context.WithTimeout(t.Context(), tc.timeout)
			defer cancel()
			writer, err := openContractFIFO(ctx, fifo, exited)
			switch tc.want {
			case "exit":
				if err == nil || !strings.Contains(err.Error(), "exited before opening") {
					t.Fatalf("early exit: %v", err)
				}
				if cmd.ProcessState.ExitCode() != 7 {
					t.Fatalf("exit = %d, want 7", cmd.ProcessState.ExitCode())
				}
			case "deadline":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("no reader: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-exited:
					t.Fatal("reader exited before writer closure")
				default:
				}
			}
			if writer != nil {
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.want == "" {
				select {
				case <-exited:
					if !cmd.ProcessState.Success() {
						t.Fatalf("reader failed: %v", cmd.ProcessState)
					}
				case <-ctx.Done():
					t.Fatal("reader failed to observe writer closure")
				}
			}
		})
	}
}

func TestOpenContractFIFOMissingPath(t *testing.T) {
	_, err := openContractFIFO(t.Context(), filepath.Join(t.TempDir(), "missing"), make(chan struct{}))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing FIFO: %v", err)
	}
}
