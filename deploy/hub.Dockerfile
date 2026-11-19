FROM node:22 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web ./
RUN npm run build

FROM golang:1.27.1 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist web/dist
RUN ldflags="-X github.com/asterism-dev/observatory/internal/version.Version=$VERSION" \
    && CGO_ENABLED=0 go build -ldflags "$ldflags" -o /hub ./cmd/hub \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$ldflags" -o /observatory-node ./cmd/node

FROM debian:trixie-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /hub /usr/local/bin/hub
COPY --from=build /observatory-node /usr/local/lib/observatory/observatory-node
ENV OBSERVATORY_NODE_BINARY=/usr/local/lib/observatory/observatory-node OBSERVATORY_LISTEN=:8090
USER 65532
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/hub"]
