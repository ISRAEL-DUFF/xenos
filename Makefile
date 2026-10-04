.PHONY: web build run test lint db
web:
	cd web && npm install && npm run build
build: web
	go build -o bin/xenos ./cmd/api
	go build -o bin/xenos-worker ./cmd/worker
run:
	go run ./cmd/api
test:
	go test ./...
lint:
	go vet ./...
db:
	docker compose up -d postgres
