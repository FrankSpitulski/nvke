// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"os"
)

type FileWriter interface {
	WriteFile(filename string, data []byte, perm os.FileMode) error
}

type OSFileWriter struct{}

func (fw *OSFileWriter) WriteFile(filename string, data []byte, perm os.FileMode) error {
	return os.WriteFile(filename, data, perm)
}
