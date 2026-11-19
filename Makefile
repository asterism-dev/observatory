.PHONY: dev-up dev-down gateway-token storage-token gateway-up gateway-down registry-env registry-up registry-down registry-gc idp-up idp-down idp-bootstrap hub cli web test run node

dev-up:
	docker compose -f deploy/dev/compose.yaml up -d --wait

dev-down:
	docker compose -f deploy/dev/compose.yaml down

gateway-token:
	grep -q '^OBSERVATORY_GATEWAY_TOKEN=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; echo "OBSERVATORY_GATEWAY_TOKEN=$$(openssl rand -hex 32)"; } >> deploy/dev/hub.env

storage-token:
	grep -q '^OBSERVATORY_STORAGE_TOKEN=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; echo "OBSERVATORY_STORAGE_TOKEN=$$(openssl rand -hex 32)"; } >> deploy/dev/hub.env

gateway-up: gateway-token storage-token
	docker compose -f deploy/dev/compose.yaml --env-file deploy/dev/hub.env --profile gateway up -d --build gateway storage

gateway-down:
	docker compose -f deploy/dev/compose.yaml --profile gateway rm -sf storage gateway modules

registry-env:
	mkdir -p deploy/dev/registry-keys
	grep -q '^OBSERVATORY_REGISTRY_EVENTS_SECRET=' deploy/dev/hub.env || { [ ! -s deploy/dev/hub.env ] || [ -z "$$(tail -c1 deploy/dev/hub.env)" ] || echo; echo "OBSERVATORY_REGISTRY_EVENTS_SECRET=$$(openssl rand -hex 32)"; echo "OBSERVATORY_REGISTRY_URL=http://localhost:55000"; echo "OBSERVATORY_REGISTRY_KEY_DIR=$(CURDIR)/deploy/dev/registry-keys"; } >> deploy/dev/hub.env

registry-up: registry-env
	docker compose -f deploy/dev/compose.yaml --env-file deploy/dev/hub.env up -d registry

registry-down:
	docker compose -f deploy/dev/compose.yaml rm -sf registry

registry-gc:
	docker compose -f deploy/dev/compose.yaml run --rm registry-gc

idp-up:
	[ -f deploy/idp/.env ] || cp deploy/idp/.env.example deploy/idp/.env
	docker compose -f deploy/idp/compose.yaml --env-file deploy/idp/.env up -d --wait

idp-down:
	docker compose -f deploy/idp/compose.yaml --env-file deploy/idp/.env down

idp-bootstrap:
	sh deploy/idp/bootstrap.sh

hub: web node
	go build -o bin/hub ./cmd/hub

cli:
	go build -o bin/observatory ./cmd/observatory

web:
	npm --prefix web ci && npm --prefix web run build && touch web/dist/.gitkeep

test:
	OBSERVATORY_TEST_DATABASE_URL=$${OBSERVATORY_TEST_DATABASE_URL:-postgres://observatory:observatory@localhost:55432/observatory?sslmode=disable} go test ./...
	node --test web/src/composables/*.test.ts

run:
	set -a; . deploy/dev/hub.env; set +a; go run ./cmd/hub

node:
	GOOS=linux GOARCH=amd64 go build -o bin/observatory-node-linux-amd64 ./cmd/node
