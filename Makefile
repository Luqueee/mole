.PHONY: build install uninstall test clean run tidy

BINARY   := mole
DIST     := dist
PKG      := ./cmd/mole
PREFIX   ?= $(or $(shell go env GOPATH 2>/dev/null),$(HOME)/go)
BINDIR   ?= $(PREFIX)/bin

# PREFIX can be overridden to install into a custom location, e.g.:
#   make install PREFIX=/usr/local
#   make install PREFIX=$(HOME)/.local
# To install to a fully-custom file path, set INSTALL_DIR instead:
INSTALL_DIR ?=

build:
	@mkdir -p $(DIST)
	go build -trimpath -o $(DIST)/$(BINARY) $(PKG)

# Install the built binary. If INSTALL_DIR is set, that exact path is
# used; otherwise $(BINDIR)/$(BINARY) (default: $(go env GOPATH)/bin/mole).
install: build
	@if [ -n "$(INSTALL_DIR)" ]; then \
		mkdir -p "$$(dirname $(INSTALL_DIR))"; \
		install -m 0755 $(DIST)/$(BINARY) $(INSTALL_DIR); \
	else \
		mkdir -p $(BINDIR); \
		install -m 0755 $(DIST)/$(BINARY) $(BINDIR)/$(BINARY); \
		echo "installed to $(BINDIR)/$(BINARY)"; \
	fi

uninstall:
	@if [ -n "$(INSTALL_DIR)" ]; then \
		rm -f $(INSTALL_DIR); \
	else \
		rm -f $(BINDIR)/$(BINARY); \
	fi

# Convenience wrappers for the cross-platform scripts/.
install.sh:
	./scripts/install.sh

uninstall.sh:
	./scripts/uninstall.sh

tidy:
	go mod tidy

test:
	go test ./...

run: build
	./$(DIST)/$(BINARY) up

clean:
	rm -rf $(DIST)
