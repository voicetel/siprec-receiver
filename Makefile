.PHONY: build test vet run

build:
	go build -o siprec-receiver ./cmd/siprec-receiver

test:
	go test ./...

vet:
	go vet ./...

run: build
	./siprec-receiver
