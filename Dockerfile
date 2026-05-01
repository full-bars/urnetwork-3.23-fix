# --- Build Stage ---
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETARCH
WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev

# Copy source
COPY . .

# Build for the target architecture with version injection
RUN GOOS=linux GOARCH=$TARGETARCH CGO_ENABLED=0 \
    go build -trimpath \
    -ldflags "-s -w -X main.Version=v3.23.0-fix.1" \
    -o provider_bin ./provider/main.go

# --- Final Stage ---
FROM alpine:latest

ARG TARGETARCH
WORKDIR /app

# Set version environment variable as a backup
ENV WARP_VERSION=v3.23.0-fix.1

# Install TechRoy's dependencies
RUN apk update && apk add --no-cache \
    tzdata iputils vnstat dos2unix \
    jq tar curl htop wget procps \
    iptables net-tools bind-tools \
    busybox-extras ca-certificates \
    ca-certificates-bundle bash \
    gosu \
  && rm -rf /var/cache/apk/*

# Setup directory structure
RUN mkdir -p /app/cgi-bin /root/.urnetwork

# Copy TechRoy's scripts from our local docker/scripts folder
COPY docker/scripts/*.sh /app/
COPY docker/scripts/stats /app/cgi-bin/

# Copy our custom compiled binary
COPY --from=builder /app/provider_bin /app/urnetwork_${TARGETARCH}_stable

# Set permissions
RUN dos2unix /app/*.sh /app/cgi-bin/stats && chmod +x /app/*.sh /app/cgi-bin/stats

# Configure vnStat (TechRoy style)
RUN sed -i \
  -e 's/^;*TimeSyncWait.*/TimeSyncWait 1/' \
  -e 's/^;*TrafficlessEntries.*/TrafficlessEntries 1/' \
  -e 's/^;*UpdateInterval.*/UpdateInterval 15/' \
  -e 's/^;*PollInterval.*/PollInterval 15/' \
  -e 's/^;*SaveInterval.*/SaveInterval 1/' \
  -e 's/^;*UnitMode.*/UnitMode 1/' \
  -e 's/^;*RateUnit.*/RateUnit 0/' \
  -e 's/^;*RateUnitMode.*/RateUnitMode 0/' \
  /etc/vnstat.conf

# Setup volumes
VOLUME ["/root/.urnetwork"]

ENTRYPOINT ["/app/entrypoint.sh"]
