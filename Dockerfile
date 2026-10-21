# nightpost — first-party observability ingest ( #145)

# Build stage: static binary, CGO off (modernc.org/sqlite is pure Go — the
# image needs no gcc/libc headers). Toolchain pinned to go.mod's requirement
# (go 1.26.0); do NOT downgrade the base image below it.
FROM golang:1.26-alpine AS build
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
# --session-secret-file keeps the session key INSIDE the volume. Without it
# the key is random per boot, so every deploy silently logs the admins out
# — the shipped default must not do that.
CMD ["--http", ":8000", "--socket", ":9913", "--db", "/data/monitor.db", \
     "--session-secret-file", "/data/session.key"]
