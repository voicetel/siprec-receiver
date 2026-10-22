.PHONY: build run fmt vet lint test coverage quality spec

build:
	go build -o siprec-receiver ./cmd/siprec-receiver

run: build
	./siprec-receiver

fmt:
	go fmt ./...

vet:
	go vet ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	elif command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "No linter found. Install golangci-lint or staticcheck." >&2; \
		exit 2; \
	fi

spec:
	@VACUUM=$$(command -v vacuum 2>/dev/null || echo "$$HOME/go/bin/vacuum"); \
	if [ -x "$$VACUUM" ]; then \
		"$$VACUUM" lint -r vacuum-ruleset.yaml internal/web/openapi.yaml; \
	else \
		echo "vacuum not found. Install: go install github.com/daveshanley/vacuum@latest" >&2; \
		exit 2; \
	fi

test:
	go test -race ./...

coverage:
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

quality: fmt vet lint coverage
	@coverage=$$(go tool cover -func=coverage.out | awk '/^total:/ {gsub(/%/, "", $$3); print $$3}'); \
	awk -v coverage="$$coverage" 'BEGIN { exit !(coverage >= 95.0) }' || \
	(echo "Coverage gate failed: expected at least 95%, got $$coverage%." >&2; exit 2)
