# FIX #5 — Reference Dockerfile for the OIDC proxy.
# Build: podman build -t oidc-proxy .
# Push:  podman push oidc-proxy registry.mydomain.no/oidc-proxy:latest
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o oidc-proxy .

FROM registry.access.redhat.com/ubi9-micro:latest
COPY --from=builder /app/oidc-proxy /usr/local/bin/oidc-proxy
USER 1001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/oidc-proxy"]
