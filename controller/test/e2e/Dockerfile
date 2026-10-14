# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The E2E suite as an image, for validating a running site from inside its
# cluster (DSX QA runs it under TestWave). It carries the whole suite; the
# entrypoint selects site-validate, the specs that change nothing on the site.
#
# Build from the REPOSITORY ROOT, so the context includes controller/api,
# which go.mod replaces with a relative path:
#   docker buildx build -f controller/test/e2e/Dockerfile .
# Dockerfile.dockerignore next to this file governs the context, not the root
# .dockerignore: it admits only the two modules, so a local e2e-config.yaml or
# run state under this directory never reaches a layer or the build cache.
#
# Bases are the controller image's, digest-pinned and refreshed by
# hack/base-image-update.sh.
ARG GOLANG_IMAGE=docker.io/golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c
ARG ALPINE_IMAGE=docker.io/alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# On the build platform, cross-compiling, so a multi-arch build needs no
# emulation.
FROM --platform=$BUILDPLATFORM ${GOLANG_IMAGE} AS builder
ENV GOPROXY=https://proxy.golang.org|direct
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY . /workspace
WORKDIR /workspace/controller/test/e2e
RUN GOWORK=off CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go test -c -trimpath -o /out/e2e.test .

# No RUN: a cross-built target needs no emulation, and alpine (rather than
# distroless) so a TestWave step can still use `script:`.
FROM ${ALPINE_IMAGE}
COPY --from=builder /out/e2e.test /usr/local/bin/e2e.test
COPY LICENSE NOTICE /licenses/

# TestWave mounts /artifacts and parses junit-test-report.xml at its root; the
# rest of the directory is uploaded as is.
WORKDIR /e2e
USER 65532:65532

LABEL org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="nvke-validate" \
      org.opencontainers.image.source="https://github.com/dsx-ai-factory/nvke" \
      org.opencontainers.image.description="NVKE site validation suite"

# The label filter and report paths are in ENTRYPOINT so that arguments a
# caller adds, such as -e2e.versions, extend the run rather than replace it.
# The filter is not the guard: the identity the image runs as is what keeps a
# mutating spec from running.
#
# -test.run because the binary also carries the module's unit tests, some of
# which need a git checkout. The JUnit report is the suite's, which lists only
# the selected specs; report.json is Ginkgo's, with every spec.
ENTRYPOINT ["/usr/local/bin/e2e.test", \
    "-test.run=^TestE2E$", \
    "-ginkgo.label-filter=site-validate", \
    "-e2e.junit-report=/artifacts/junit-test-report.xml", \
    "-ginkgo.json-report=/artifacts/report.json", \
    "-ginkgo.silence-skips", \
    "-ginkgo.no-color", \
    "-ginkgo.v"]
