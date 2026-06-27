.PHONY: build test run

build:
	go build -o bin/webdavs3.exe ./cmd/webdavs3

test:
	go test ./... -v -race -count=1

run: build
	./bin/webdavs3.exe
