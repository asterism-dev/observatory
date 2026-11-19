FROM --platform=$BUILDPLATFORM golang:1.27.1 AS build
ARG VERSION=dev TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/storage cmd/storage
COPY internal internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-X github.com/asterism-dev/observatory/internal/version.Version=$VERSION" -o /storage ./cmd/storage

FROM debian:trixie-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nbdkit \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /storage /usr/local/bin/storage
VOLUME /disks
# storage stays PID 1: docker stop signals only it, and its exit SIGKILLs every nbdkit (never a graceful nbdkit stop)
ENTRYPOINT ["sh", "-c", "exec storage -hub \"${HUB_URL:-http://host.docker.internal:8090}\""]
