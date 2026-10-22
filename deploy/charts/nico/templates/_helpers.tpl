{{- /*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
*/ -}}

{{- define "nvke-nico.database.password" -}}
{{- required "database.password is required. Set it with --set database.password=... or supply database.existingSecret." .Values.database.password -}}
{{- end -}}

{{/*
The Secret nico-api reads its database credentials from.

Read from the NICo subchart's own values, not written independently, so the
name this chart creates and the name nico-api mounts cannot drift apart. NICo
prefixes the convention with the namespace, which the chart's default hardcodes
to one this deployment does not use.
*/}}
{{- define "nvke-nico.database.secretName" -}}
{{- if .Values.database.secretName -}}
{{- .Values.database.secretName -}}
{{- else -}}
{{- $name := dig "nico-api" "envFrom" "databaseCredentials" "secretName" "" .Values.nico -}}
{{- if not $name -}}
{{- fail "cannot resolve the database Secret name: set either database.secretName or nico.nico-api.envFrom.databaseCredentials.secretName" -}}
{{- end -}}
{{- $name -}}
{{- end -}}
{{- end -}}

{{- define "nvke-nico.database.uri" -}}
{{- $d := .Values.database -}}
{{- printf "postgres://%s:%s@%s:%v/%s?sslmode=%s" $d.user (include "nvke-nico.database.password" . | urlquery) $d.host $d.port $d.dbName $d.sslMode -}}
{{- end -}}
