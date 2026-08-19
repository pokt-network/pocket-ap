# syntax=docker/dockerfile:1

# pocket-ap holds a staked app's private key and every relay spends stake, so
# this image is deliberately minimal: no shell, no package manager, nothing to
# pivot to if the process is ever compromised.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache the module tree separately: it is ~929 modules and dominates the build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=docker
ARG COMMIT=none
ARG DATE=unknown
# CGO off: the whole dep tree is pure Go, which is what allows FROM scratch below.
# -s -w matters here — unstripped this binary is ~152 MB, stripped ~103 MB.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /pocket-ap ./cmd/pocket-ap

FROM scratch
# TLS roots: every full node and supplier is reached over HTTPS, and scratch has
# no CA bundle — without this every relay fails x509 verification.
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
