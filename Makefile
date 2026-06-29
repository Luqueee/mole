.PHONY: build install test clean run tidy

BINARY := fallback-proxy
DIST   := dist
PKG    := ./cmd/fallback-proxy

build:
	@mkdir -p $(DIST)
	go build -trimpath -o $(DIST)/$(BINARY) $(PKG)

install: build
	install -m 0755 $(DIST)/$(BINARY) $(GOPATH)/bin/$(BINARY)

tidy:
	go mod tidy

test:
	go test ./...

run: build
	./$(DIST)/$(BINARY) up

clean:
	rm -rf $(DIST)