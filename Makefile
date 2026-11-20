APP_NAME := nexus
BIN_DIR  := bin

.PHONY: all build run fmt vet tidy test clean

all: build

build:
	go build -o $(BIN_DIR)/$(APP_NAME) ./cmd/nexus

run:
	go run ./cmd/nexus

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

test:
	go test ./...

clean:
	go clean
	rm -rf $(BIN_DIR)
