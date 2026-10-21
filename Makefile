.PHONY: build test vet run spec

build:
	go build -o siprec-receiver ./cmd/siprec-receiver

spec:
	@VACUUM=$$(command -v vacuum 2>/dev/null || echo "$$HOME/go/bin/vacuum"); \
	if [ -x "$$VACUUM" ]; then \
		"$$VACUUM" lint -r vacuum-ruleset.yaml internal/web/openapi.yaml; \
	else \
		echo "vacuum not found. Install: go install github.com/daveshanley/vacuum@latest" >&2; \
		exit 2; \
	fi

test:
	go test ./...

vet:
	go vet ./...

run: build
	./siprec-receiver
