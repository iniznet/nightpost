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
