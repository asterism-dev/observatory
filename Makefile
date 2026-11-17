.PHONY: dev-up dev-down idp-up idp-down idp-bootstrap hub cli web test run node

dev-up:
	docker compose -f deploy/dev/compose.yaml up -d --wait

dev-down:
	docker compose -f deploy/dev/compose.yaml down

idp-up:
	docker compose -f deploy/dev/zitadel/compose.yaml --env-file deploy/dev/zitadel/.env up -d --wait

idp-down:
	docker compose -f deploy/dev/zitadel/compose.yaml --env-file deploy/dev/zitadel/.env down

idp-bootstrap:
	sh deploy/dev/zitadel/bootstrap.sh

hub: web
	go build -o bin/hub ./cmd/hub

cli:
	go build -o bin/observatory ./cmd/observatory

web:
	npm --prefix web ci && npm --prefix web run build && touch web/dist/.gitkeep

test:
	OBSERVATORY_TEST_DATABASE_URL=$${OBSERVATORY_TEST_DATABASE_URL:-postgres://observatory:observatory@localhost:55432/observatory?sslmode=disable} go test ./...

run:
	set -a; . deploy/dev/hub.env; set +a; go run ./cmd/hub

node:
	GOOS=linux GOARCH=amd64 go build -o bin/observatory-node-linux-amd64 ./cmd/node
