.PHONY: web build run test lint db sqlc release e2e ci
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
# Browser journey test; needs the stack running with the fakes (see web/e2e/README.md).
e2e:
	cd web && npm run e2e

# The same checks CI runs (needs XENOS_TEST_DATABASE_URL for the integration tests; see README).
ci:
	@out=$$(gofmt -l cmd internal web/embed.go); if [ -n "$$out" ]; then echo "Not gofmt-formatted:"; echo "$$out"; exit 1; fi
	go vet ./...
	sqlc diff
	go test ./... -count=1
	cd web && npx tsc --noEmit && npm run build
