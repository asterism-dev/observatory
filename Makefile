.PHONY: gen gen-check lint dev-up dev-down gateway-token storage-token edge-token service-token gateway-up gateway-down registry-env registry-up registry-down telemetry-env telemetry-up telemetry-down nats-token nats-up nats-down idp-up idp-down idp-bootstrap observatory cli web test run node

dev-up:
	docker compose -f deploy/dev/compose.yaml up -d --wait

dev-down:
	docker compose -f deploy/dev/compose.yaml down

gateway-token:
	grep -q '^OBSERVATORY_GATEWAY_TOKEN=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; echo "OBSERVATORY_GATEWAY_TOKEN=$$(openssl rand -hex 32)"; } >> deploy/dev/hub.env

storage-token:
	grep -q '^OBSERVATORY_STORAGE_TOKEN=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; echo "OBSERVATORY_STORAGE_TOKEN=$$(openssl rand -hex 32)"; } >> deploy/dev/hub.env

edge-token:
	grep -q '^OBSERVATORY_EDGE_TOKEN=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; echo "OBSERVATORY_EDGE_TOKEN=$$(openssl rand -hex 32)"; } >> deploy/dev/hub.env

service-token:
	grep -q '^OBSERVATORY_SERVICE_TOKENS\{0,1\}=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; t=$$(openssl rand -hex 32); echo "OBSERVATORY_SERVICE_TOKEN=$$t"; echo "OBSERVATORY_SERVICE_TOKENS=edge=$$t"; } >> deploy/dev/hub.env

gateway-up: gateway-token storage-token
	docker compose -f deploy/dev/compose.yaml --env-file deploy/dev/hub.env --profile gateway up -d --build gateway storage

gateway-down:
	docker compose -f deploy/dev/compose.yaml --profile gateway rm -sf storage gateway modules

registry-env:
	mkdir -p deploy/dev/registry-keys deploy/dev/registry-certs
	grep -q '^OBSERVATORY_REGISTRY_EVENTS_SECRET=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; echo "OBSERVATORY_REGISTRY_EVENTS_SECRET=$$(openssl rand -hex 32)"; echo "OBSERVATORY_REGISTRY_URL=http://localhost:55000"; echo "OBSERVATORY_REGISTRY_KEY_DIR=$(CURDIR)/deploy/dev/registry-keys"; echo "OBSERVATORY_REGISTRY_CERT_DIR=$(CURDIR)/deploy/dev/registry-certs"; } >> deploy/dev/hub.env

registry-up: registry-env
	docker compose -f deploy/dev/compose.yaml --env-file deploy/dev/hub.env up -d registry

registry-down:
	docker compose -f deploy/dev/compose.yaml rm -sf registry

telemetry-env:
	grep -q '^OBSERVATORY_METRICS_URL=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; echo "OBSERVATORY_METRICS_URL=http://localhost:58428"; echo "OBSERVATORY_LOGS_URL=http://localhost:59428"; } >> deploy/dev/hub.env

telemetry-up: telemetry-env
	docker compose -f deploy/dev/compose.yaml --profile telemetry up -d --wait victoriametrics victorialogs

telemetry-down:
	docker compose -f deploy/dev/compose.yaml rm -sf victoriametrics victorialogs

nats-token:
	grep -q '^OBSERVATORY_NATS_TOKEN=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; echo "OBSERVATORY_NATS_TOKEN=$$(openssl rand -hex 32)"; echo "OBSERVATORY_NATS_URL=nats://localhost:54222"; } >> deploy/dev/hub.env

nats-up: nats-token
	docker compose -f deploy/dev/compose.yaml --env-file deploy/dev/hub.env --profile nats up -d --wait nats

nats-down:
	docker compose -f deploy/dev/compose.yaml rm -sf nats

idp-up:
	[ -f deploy/idp/.env ] || cp deploy/idp/.env.example deploy/idp/.env
	docker compose -f deploy/idp/compose.yaml --env-file deploy/idp/.env up -d --wait

idp-down:
	docker compose -f deploy/idp/compose.yaml --env-file deploy/idp/.env down

idp-bootstrap:
	sh deploy/idp/bootstrap.sh

observatory: web node
	go build -o bin/observatory ./cmd/observatory

cli:
	go build -o bin/observatory ./cmd/observatory

web:
	npm --prefix web ci && npm --prefix web run build && touch web/dist/.gitkeep

test:
	OBSERVATORY_TEST_DATABASE_URL=$${OBSERVATORY_TEST_DATABASE_URL:-postgres://observatory:observatory@localhost:55432/observatory?sslmode=disable} go test ./...
	node --test web/src/composables/*.test.ts

run: edge-token
	set -a; . deploy/dev/hub.env; set +a; OBSERVATORY_HUB_LISTEN=$${OBSERVATORY_HUB_LISTEN:-:8091} OBSERVATORY_DATA_DIR=$(PWD)/.data go run ./cmd/observatory serve all

node:
	GOOS=linux GOARCH=amd64 go build -o bin/observatory-node-linux-amd64 ./cmd/node

gen:
	test -x web/node_modules/.bin/protoc-gen-es || npm --prefix web ci
	go tool -modfile=tools/go.mod buf generate

gen-check: gen
	git diff --exit-code -- gen web/src/gen
	test -z "$$(git ls-files --others --exclude-standard -- gen web/src/gen)"

lint:
	go tool -modfile=tools/go.mod golangci-lint run
	go tool -modfile=tools/go.mod buf lint
