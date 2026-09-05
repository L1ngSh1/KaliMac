BINARY := ./bin/km

.PHONY: build test vet fmt clean
build:
	go build -o $(BINARY) ./cmd/km

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin

all: vet test build
