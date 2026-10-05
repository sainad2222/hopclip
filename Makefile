VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test lint vuln docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/hopclip ./cmd/hopclip

run:
	ADMIN_USERNAME=admin ADMIN_PASSWORD=devpassword DATA_DIR=./data go run ./cmd/hopclip

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t ghcr.io/sainad2222/hopclip:$(VERSION) .

clean:
	rm -rf bin data
