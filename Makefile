.PHONY: web build run test lint db sqlc release
web:
	cd web && npm install && npm run build
build: web
	go build -o bin/xenos ./cmd/api
	go build -o bin/xenos-worker ./cmd/worker
	go build -o bin/xenosctl ./cmd/xenosctl

# Static linux/amd64 binaries for the control-plane VM (the dashboard is embedded).
release: web
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/linux-amd64/xenos ./cmd/api
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/linux-amd64/xenos-worker ./cmd/worker
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/linux-amd64/xenosctl ./cmd/xenosctl
run:
	go run ./cmd/api
test:
	go test ./...
lint:
	go vet ./...
db:
	docker compose up -d postgres
sqlc:
	sqlc generate
