{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}

{{/*
Non-empty when the cluster already has a default StorageClass.

Reads the live cluster, so it is empty during `helm template` and `--dry-run`.
NOTES.txt uses it to warn, never to refuse.
*/}}
{{- define "nvke-nico-stack.hasDefaultStorageClass" -}}
{{- range $sc := (lookup "storage.k8s.io/v1" "StorageClass" "" "").items -}}
{{- if eq (index $sc.metadata.annotations "storageclass.kubernetes.io/is-default-class" | default "false") "true" -}}
{{- $sc.metadata.name -}}
{{- end -}}
{{- end -}}
{{- end -}}
