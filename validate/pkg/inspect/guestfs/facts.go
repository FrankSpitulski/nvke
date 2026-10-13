// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package guestfs

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

// The Session implements checks.ImageFS. Fact calls use a background context:
// each is one short remote command against an already-booted appliance.
var _ checks.ImageFS = (*Session)(nil)

// Exists reports whether a path exists (any file type).
func (s *Session) Exists(path string) (bool, error) {
	out, err := s.remote(context.Background(), "exists", path)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// Cat returns a file's full contents.
func (s *Session) Cat(path string) (string, error) {
	return s.remote(context.Background(), "cat", path)
}

// ReadLink returns a symlink's target.
func (s *Session) ReadLink(path string) (string, error) {
	out, err := s.remote(context.Background(), "readlink", path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Stat returns permission bits and ownership for a path.
func (s *Session) Stat(path string) (checks.FileStat, error) {
	out, err := s.remote(context.Background(), "statns", path)
	if err != nil {
		return checks.FileStat{}, err
	}
	return parseStatNS(out)
}

// HasLUKS reports whether the image carried a LUKS container.
func (s *Session) HasLUKS() bool { return s.hasLUKS }

// parseStatNS reads the fields checks need out of guestfish `statns` output,
// which prints one "name: value" per line with st_mode in decimal.
func parseStatNS(out string) (checks.FileStat, error) {
	var st checks.FileStat
	seen := 0
	for _, line := range strings.Split(out, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v := strings.TrimSpace(value)
		switch strings.TrimSpace(name) {
		case "st_mode":
			mode, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return st, fmt.Errorf("statns st_mode %q: %w", v, err)
			}
			st.Perm = uint32(mode) & 0o7777
			seen++
		case "st_uid":
			uid, err := strconv.Atoi(v)
			if err != nil {
				return st, fmt.Errorf("statns st_uid %q: %w", v, err)
			}
			st.UID = uid
			seen++
		case "st_gid":
			gid, err := strconv.Atoi(v)
			if err != nil {
				return st, fmt.Errorf("statns st_gid %q: %w", v, err)
			}
			st.GID = gid
			seen++
		}
	}
	if seen != 3 {
		return st, fmt.Errorf("statns output missing st_mode/st_uid/st_gid: %q", out)
	}
	return st, nil
}
