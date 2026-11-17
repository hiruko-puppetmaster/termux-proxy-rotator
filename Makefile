BINARY ?= rotator
GO     ?= go

.PHONY: build test test-race vet run clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BINARY) .

test:
	$(GO) test -count=1 ./...

# The Go race detector is not supported on android/arm64. Run this on a
# desktop checkout instead of inside Termux.
test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

run: build
	./$(BINARY) -config config.json

clean:
	rm -f $(BINARY)
