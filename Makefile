GO ?= go
PREFIX ?= $(HOME)/.local
BINDIR := $(PREFIX)/bin
BIN := erma
SCRIPTS := erma-hook erma-open erma-sway-focus erma-ctx
SETTINGS := $(HOME)/.claude/settings.json

.PHONY: build install uninstall test
.DEFAULT_GOAL := build

build:
	$(GO) build -o $(BIN) ./cmd/erma

install: build
	install -d $(BINDIR)
	install -m 0755 $(BIN) $(BINDIR)/
	install -m 0755 $(addprefix contrib/,$(SCRIPTS)) $(BINDIR)/
	@if [ -f "$(SETTINGS)" ] && ! grep -q erma-hook "$(SETTINGS)"; then \
		echo "warning: $(SETTINGS) does not reference erma-hook — Claude Code hooks are not wired. See README Setup."; \
	fi

uninstall:
	rm -f $(addprefix $(BINDIR)/,$(BIN) $(SCRIPTS))

test:
	$(GO) vet ./...
	$(GO) test ./...
