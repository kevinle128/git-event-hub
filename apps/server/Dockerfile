FROM golang:1.26-alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# Production uses Postgres only, so the binary needs no cgo (SQLite is for tests).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/server ./cmd/server

FROM alpine:3.20 AS app

RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 hub

COPY --from=builder /out/server /app/server

USER hub
ENV HUB_CONFIG=/etc/git-event-hub/config.yaml
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/app/server", "-healthcheck", "http://127.0.0.1:8080/healthz"]

ENTRYPOINT ["/app/server"]
