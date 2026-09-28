# Build stage.
#
# CGO is off so the result is a static binary that runs on a scratch base.
# That matters here beyond image size: this container holds live account
# cookies, and a scratch image has no shell and no package manager for an
# attacker to pivot through if the process is ever compromised.
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses this layer.
COPY go.mod ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/mimowebapi .

# Runtime stage.
FROM scratch

# TLS roots are required for the upstream HTTPS call, and scratch ships none.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/mimowebapi /mimowebapi

# The container has no shell, so the health check is this binary asking the
# service whether it is up. It exits non-zero when the local listener is not
# answering, which is what Docker's health check reads.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/mimowebapi", "-config", "/data/config.json", "-healthcheck"]

WORKDIR /data
EXPOSE 8793

ENTRYPOINT ["/mimowebapi"]
CMD ["-config", "/data/config.json"]
