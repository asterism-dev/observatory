ARG GUESTBOOT_TAG=6.18.64-a4c07d7d

FROM --platform=linux/amd64 ghcr.io/asterism-dev/observatory-guestboot:${GUESTBOOT_TAG} AS guestboot

FROM --platform=$BUILDPLATFORM node:22.23.5 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.1 AS build
ARG VERSION=dev TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist web/dist
RUN ldflags="-X github.com/asterism-dev/observatory/internal/version.Version=$VERSION" \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "$ldflags" -o /hub ./cmd/hub \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$ldflags" -o /observatory-node ./cmd/node \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$ldflags" -o /guestboot/observatory-init ./cmd/init
COPY --from=guestboot /vmlinux /mke2fs /resize2fs /guestboot/
RUN echo "$VERSION" > /guestboot/VERSION \
    && tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf /guestboot.tar -C /guestboot VERSION mke2fs observatory-init resize2fs vmlinux

FROM debian:trixie-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && install -d -o 65532 /keys
COPY --from=build /hub /usr/local/bin/hub
COPY --from=build /observatory-node /usr/local/lib/observatory/observatory-node
COPY --from=build /guestboot.tar /usr/local/lib/observatory/guestboot.tar
ENV OBSERVATORY_NODE_BINARY=/usr/local/lib/observatory/observatory-node OBSERVATORY_GUESTBOOT=/usr/local/lib/observatory/guestboot.tar OBSERVATORY_LISTEN=:8090
USER 65532
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/hub"]
