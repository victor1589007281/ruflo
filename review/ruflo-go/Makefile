.PHONY: build test lint clean install

build:
	go build -o bin/ruflo ./cmd/ruflo

test:
	go test ./...

lint:
	golangci-lint run

clean:
	rm -rf bin/

install:
	go install ./cmd/ruflo
