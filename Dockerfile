# --- Build Stage ---
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETARCH
ARG VERSION=v.unknown
WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev

# Copy module manifests first so dependency download is a separate
# cacheable layer (only invalidated when go.mod/go.sum change)
COPY go.mod go.sum ./

# Download modules once (cached across builds unless manifests change)
RUN go mod download

# Copy the rest of the source
COPY . .

# Build for the target architecture with version injection
RUN GOOS=linux GOARCH=$TARGETARCH CGO_ENABLED=0 \
    go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o provider_bin ./provider/

# --- Final Stage ---
FROM alpine:latest

ARG TARGETARCH
ARG VERSION=v.unknown
WORKDIR /app

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
# State dir: scripts resolve state at $HOME/.urnetwork so a container run
# with a different HOME (e.g. Pelican eggs) keeps state on its own volume.
RUN mkdir -p /app/cgi-bin

# Copy TechRoy's scripts from our local docker/scripts folder
COPY docker/scripts/*.sh /app/
COPY docker/scripts/stats /app/cgi-bin/

# Copy our custom compiled binary
COPY --from=builder /app/provider_bin /app/urnetwork_${TARGETARCH}_stable

# Set permissions
RUN dos2unix /app/*.sh /app/cgi-bin/stats && chmod +x /app/*.sh /app/cgi-bin/stats

# Expose the proxy-health helper on PATH for `docker exec <c> proxy-health`
RUN ln -sf /app/proxy-health.sh /usr/local/bin/proxy-health
RUN ln -sf /app/proxy-traffic.sh /usr/local/bin/proxy-traffic
RUN ln -sf /app/logs.sh /usr/local/bin/logs
RUN ln -sf /app/urnet-tools.sh /usr/local/bin/urnet-tools

# Expose the provider binary on PATH as `provider` for `docker exec <c> provider <cmd>`
RUN ln -sf /app/urnetwork_${TARGETARCH}_stable /usr/local/bin/provider

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
# State volume: fixed path so operators can mount a single known location.
# Under Pelican the panel writes $URNETWORK_STATE_DIR anyway; keeping the
# VOLUME declaration at /root/.urnetwork preserves default-Docker behavior.
VOLUME ["/root/.urnetwork"]

ENTRYPOINT ["/app/entrypoint.sh"]
