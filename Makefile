.PHONY: fmt vet tidy test race

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

test:
	go test ./...

race:
	go test ./... -race
