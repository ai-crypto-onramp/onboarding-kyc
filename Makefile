.PHONY: build test run lint docker-build docker-run migrate-up migrate-down clean

build:
	go build -o bin/server ./cmd/onboarding-kyc

test:
	go test ./cmd/... ./internal/... -race -coverprofile=coverage.out -coverpkg=./cmd/...,./internal/...

run:
	go run ./cmd/onboarding-kyc

lint:
	golangci-lint run

docker-build:
	docker build -t ai-crypto-onramp/onboarding-kyc .

docker-run:
	docker run --rm -p 8080:8080 ai-crypto-onramp/onboarding-kyc

migrate-up:
	go run ./cmd/migrate --up

migrate-down:
	go run ./cmd/migrate --down

clean:
	rm -rf bin/ coverage.out
