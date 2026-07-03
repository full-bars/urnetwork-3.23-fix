# --- Build Stage ---
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETARCH
ARG VERSION=v.unknown
WORKDIR /app

# Cached as its own layer: only busts when go.mod/go.sum change, not on
# every hub/*.go edit (which is what actually triggers this build).
COPY go.mod go.sum ./
RUN go mod download

# hub uses modernc.org/sqlite (pure Go, no cgo) so no gcc/musl-dev needed here,
# and hub/ imports nothing else from this module, so only it needs copying.
COPY hub/ ./hub/

RUN GOOS=linux GOARCH=$TARGETARCH CGO_ENABLED=0 \
    go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o hub_bin ./hub/

# --- Final Stage ---
FROM alpine:latest

WORKDIR /app

RUN apk add --no-cache ca-certificates && rm -rf /var/cache/apk/*

COPY --from=builder /app/hub_bin /app/hub

# hub.db, tls.crt/tls.key, and snapshot data all live here
RUN mkdir -p /data
VOLUME ["/data"]

EXPOSE 8080 8443

ENTRYPOINT ["/app/hub", "-addr", ":8080", "-data", "/data"]
