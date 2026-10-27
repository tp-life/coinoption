BINARY := coinoption
PKG := github.com/tp/coinoption/cmd/coinoption

.PHONY: build test vet fmt smoke

build:
	go build -o bin/$(BINARY) $(PKG)

test:
	go test ./... -race

vet:
	go vet ./...

fmt:
	gofmt -w .

smoke:
	go run ./cmd/coinoption --smoke --uly BTC-USDT
