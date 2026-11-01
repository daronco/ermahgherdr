GO ?= go
PREFIX ?= $(HOME)/.local
BINDIR := $(PREFIX)/bin
BIN := erma
SCRIPTS := erma-hook erma-open erma-sway-focus erma-ctx
SETTINGS := $(HOME)/.claude/settings.json

UNIT := erma-serve.service
UNIT_SRC := contrib/$(UNIT)
UNIT_DST := $(HOME)/.config/systemd/user/$(UNIT)
SERVE_UPSTREAM := http://127.0.0.1:8731
SERVE_HTTPS := 443

.PHONY: build install uninstall install-serve uninstall-serve test
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

install-serve: install
	install -d $(dir $(UNIT_DST))
	install -m 0644 $(UNIT_SRC) $(UNIT_DST)
	systemctl --user daemon-reload
	systemctl --user enable --now $(UNIT)
	@if ! tailscale serve status --json 2>/dev/null | grep -q "$(SERVE_UPSTREAM)"; then \
		if ! tailscale serve --bg --https=$(SERVE_HTTPS) $(SERVE_UPSTREAM); then \
			echo; \
			echo "If the error above says sudo is required, run ONE of:"; \
			echo "  sudo tailscale serve --bg --https=$(SERVE_HTTPS) $(SERVE_UPSTREAM)"; \
			echo "  sudo tailscale set --operator=\$$USER   # one-time; then make install-serve again"; \
		fi; \
	fi

uninstall-serve:
	-systemctl --user disable --now $(UNIT)
	-rm -f $(UNIT_DST)
	-systemctl --user daemon-reload
	@echo "To stop the Tailscale proxy, run: tailscale serve --https=$(SERVE_HTTPS) off"

test:
	$(GO) vet ./...
	$(GO) test ./...
