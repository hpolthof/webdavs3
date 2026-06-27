.PHONY: build test run

build:
	go build -o bin/webdav3s.exe ./cmd/webdav3s

test:
	go test ./... -v -race -count=1

run: build
	./bin/webdav3s.exe
