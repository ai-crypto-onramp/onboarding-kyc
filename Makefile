.PHONY: build test run lint cover docker-build docker-run migrate-up migrate-down clean

build:
	go build -o bin/onboarding-kyc ./cmd/onboarding-kyc

test:
	go test ./internal/... -race -coverprofile=coverage.out -coverpkg=./internal/...

run:
	go run ./cmd/onboarding-kyc

lint:
	golangci-lint run

cover: test
	go tool cover -func=coverage.out | tail -1

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
