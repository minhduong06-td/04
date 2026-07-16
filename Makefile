.PHONY: dev down migrate fmt lint test integration-test logs

dev:
	docker compose up --build -d

down:
	docker compose down

migrate:
	docker compose exec -T api /app/api -migrate

fmt:
	go fmt ./...

lint:
	go vet ./...

test:
	go test -v -count=1 -short ./...

integration-test:
	go test -v -count=1 -tags=integration ./tests/...

logs:
	docker compose logs -f
