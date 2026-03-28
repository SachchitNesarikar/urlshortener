.PHONY: dev db db-stop build run test

# Start databases via Docker
db:
	docker-compose up -d
	@echo "⏳ Waiting for Postgres..."
	@sleep 2

# Stop databases
db-stop:
	docker-compose down

# Download dependencies
deps:
	go mod download

# Run in development mode
dev: deps
	go run main.go

# Build binary
build:
	go build -o urlshortener main.go

# Run binary
run: build
	./urlshortener

# Quick API smoke test (requires running server)
test:
	@echo "--- Create short URL ---"
	curl -s -X POST http://localhost:8080/api/shorten \
		-H "Content-Type: application/json" \
		-d '{"url":"https://github.com/golang/go"}' | jq .
	@echo ""
	@echo "--- List all URLs ---"
	curl -s http://localhost:8080/api/urls | jq .
