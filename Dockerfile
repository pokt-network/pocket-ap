# syntax=docker/dockerfile:1

# pocket-ap holds a staked app's private key and every relay spends stake, so
# this image is deliberately minimal: no shell, no package manager, nothing to
# pivot to if the process is ever compromised.

# --platform=$BUILDPLATFORM pins the BUILDER to the machine doing the building,
# and the build below then cross-compiles to $TARGETARCH. Without it, buildx runs
# this whole stage inside an emulated container for every non-native platform —
# which means compiling a 929-module cosmos-sdk / cometbft / go-ethereum tree
# under QEMU. That took over 40 minutes for the linux/arm64 leg of v0.1.1 while
# the native leg finished in minutes. Go cross-compiles this tree natively with
# CGO off, so emulation buys nothing whatsoever here.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# Cache the module tree separately: it is ~929 modules and dominates the build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=docker
ARG COMMIT=none
ARG DATE=unknown
# Set by buildx from the platform being built, NOT by us. They are what makes the
# builder pin above a cross-compile rather than a native build repeated twice.
ARG TARGETOS
ARG TARGETARCH
# CGO off: the whole dep tree is pure Go, which is what allows FROM scratch below
# and what makes the cross-compile work without a toolchain per architecture.
# -s -w matters here — unstripped this binary is ~152 MB, stripped ~103 MB.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /pocket-ap ./cmd/pocket-ap

FROM scratch
# TLS roots: every full node and supplier is reached over HTTPS, and scratch has
# no CA bundle — without this every relay fails x509 verification. This copies
# from the builder, which is now the BUILD platform rather than the target one; a
# CA bundle is architecture-neutral data, so that is correct either way.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /pocket-ap /pocket-ap
COPY config.example.yaml /etc/pocket-ap/config.example.yaml

# Non-root. scratch has no /etc/passwd, so this is a bare uid.
USER 65534:65534

# The relay listener and the admin listener. Both bind loopback INSIDE the
# container by default, which does not reach the host — so a container config
# must bind 0.0.0.0 and rely on Docker's port mapping for the boundary instead.
# That is the one place the "always bind loopback" rule does not apply.
EXPOSE 8545 9090

ENTRYPOINT ["/pocket-ap"]
CMD ["serve", "-config", "/etc/pocket-ap/config.yaml"]
