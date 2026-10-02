.PHONY: build test clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o oglinda .

test:
	go vet ./...
	go test ./...

clean:
	rm -f oglinda
