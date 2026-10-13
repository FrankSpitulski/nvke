// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package deploypath exposes the deployment metadata used by nvkectl and its
// deploy executor. Parsing, validation and serialization share nvkectl's owner.
package deploypath

import owner "github.com/dsx-ai-factory/nvke/cli/internal/deploypath"

// Item describes an item in the deployment order.
type Item = owner.Item

// Release describes an item's chart, Helm release, values and prerequisites.
type Release = owner.Release

// TypeHelm identifies a chart item.
const TypeHelm = owner.TypeHelm

// Items reads and validates the ordered item list under dir.
func Items(dir string) ([]Item, error) { return owner.Items(dir) }

// ReadRelease reads and validates an item's deployment metadata under itemDir.
func ReadRelease(itemDir string) (*Release, error) { return owner.ReadRelease(itemDir) }

// MarshalItems serializes the item list in the deploy executor's grammar.
func MarshalItems(items []Item, header string) []byte { return owner.MarshalItems(items, header) }
