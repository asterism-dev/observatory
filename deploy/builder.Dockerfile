FROM --platform=$BUILDPLATFORM golang:1.27.1 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/build-agent cmd/build-agent
COPY internal internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X github.com/asterism-dev/observatory/internal/version.Version=$VERSION" -o /build-agent ./cmd/build-agent

FROM --platform=linux/amd64 debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]
ARG BUILDKIT_VERSION=v0.34.2
ARG BUILDKIT_SHA256=b9f18b8e37eb450e9e5df03117045237cdd12d3c4a0034ffeeaa857852a15e13
RUN apt-get -y -q update \
 && DEBIAN_FRONTEND=noninteractive apt-get -y -q install --no-install-recommends ca-certificates curl \
 && curl -fsSL --proto =https --proto-redir =https --retry 3 -o /tmp/buildkit.tar.gz \
    "https://github.com/moby/buildkit/releases/download/${BUILDKIT_VERSION}/buildkit-${BUILDKIT_VERSION}.linux-amd64.tar.gz" \
 && echo "${BUILDKIT_SHA256}  /tmp/buildkit.tar.gz" | sha256sum -c --quiet - \
 && tar -xzf /tmp/buildkit.tar.gz -C /usr/local bin/buildkitd bin/buildctl bin/buildkit-runc \
 && apt-get -y -q purge curl && apt-get -y -q autoremove \
 && rm -rf /var/lib/apt/lists/* /tmp/*
COPY --from=build /build-agent /usr/local/bin/build-agent
RUN buildctl --version | grep -F "$BUILDKIT_VERSION" && buildkitd --version | grep -F "$BUILDKIT_VERSION"
ENTRYPOINT ["/usr/local/bin/build-agent"]
