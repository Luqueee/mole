.PHONY: build install test clean run tidy

BINARY := mole
DIST   := dist
PKG    := ./cmd/mole

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