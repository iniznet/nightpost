<<<<<<< HEAD
# syntax=docker/dockerfile:1

# fenrirealm-monitor — single static binary, config via CLI flags only
# (no env reads at runtime; see .env.example for the flag contract).

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/monitor ./cmd/monitor

FROM alpine:3.21
# Non-root runtime user; busybox wget powers the /up healthcheck.
RUN adduser -D -H monitor
COPY --from=build /out/monitor /monitor
USER monitor
EXPOSE 8000 9913
VOLUME /data
ENTRYPOINT ["/monitor"]
# -db MUST live on the persistent volume (single-instance sqlite — no replicas).
CMD ["-db", "/data/monitor.db", "-http", ":8000", "-socket", ":9913"]
=======
# fenrirealm-monitor — first-party observability ingest (iniznet/fenrirealm-tracker#141 #145)

# Build stage: static binary, CGO off (modernc.org/sqlite is pure Go — the
# image needs no gcc/libc headers, mirroring the vendor-junction-safe posture
# documented for the PHP side).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/monitor ./cmd/monitor

# Runtime stage: alpine only for tzdata + ca-certificates; the server itself
# is the single static binary. Runs unprivileged; state lives in /data.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S monitor \
    && adduser -S -G monitor -h /data monitor \
    && mkdir -p /data && chown monitor:monitor /data
COPY --from=build /out/monitor /usr/local/bin/monitor

USER monitor
WORKDIR /data
VOLUME /data

# UI + Sentry/profile/error/xhprof ingest on one HTTP port, Monolog
# newline-JSON on TCP 9913. Same env-name surface the senders use.
EXPOSE 8000 9913

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8000/up || exit 1

ENTRYPOINT ["/usr/local/bin/monitor"]
CMD ["--http", ":8000", "--socket", ":9913", "--db", "/data/monitor.db"]
>>>>>>> d6fc871 (feat(deploy): Dockerfile + compose service — loopback UI, dokploy-network senders, persistent volume (#145))
