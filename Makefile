BINARY   := agent-relay
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -s -w -X main.Version=$(VERSION)
GOFLAGS  := CGO_ENABLED=1

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  BIN_DIR     := /usr/local/bin
  PLIST_DIR   := $(HOME)/Library/LaunchAgents
  PLIST_FILE  := com.agent-relay.plist
else
  BIN_DIR     := $(HOME)/.local/bin
  UNIT_DIR    := $(HOME)/.config/systemd/user
  UNIT_FILE   := agent-relay.service
endif

SKILL_DIR  := $(HOME)/.claude/commands
SKILL_FILE := relay.md

.PHONY: build test vet install uninstall service service-stop skill clean help

## build: Compile the binary with version info
build:
	$(GOFLAGS) go build -tags "fts5" -ldflags="$(LDFLAGS)" -o $(BINARY) .

## test: Run the test suite with the fts5 build tag
test:
	$(GOFLAGS) go test -tags "fts5" ./...

## vet: Run go vet with the fts5 build tag
vet:
	$(GOFLAGS) go vet -tags "fts5" ./...

## install: Build, install binary, skill, and service
install: build skill
ifeq ($(UNAME_S),Darwin)
	@if [ -w "$(BIN_DIR)" ]; then \
		install -m 755 $(BINARY) $(BIN_DIR)/$(BINARY); \
	else \
		sudo install -m 755 $(BINARY) $(BIN_DIR)/$(BINARY); \
	fi
else
	@mkdir -p $(BIN_DIR)
	install -m 755 $(BINARY) $(BIN_DIR)/$(BINARY)
endif
	@$(MAKE) service
	@echo "Installed $(BINARY) $(VERSION)"

## uninstall: Stop service, remove binary and skill
uninstall: service-stop
ifeq ($(UNAME_S),Darwin)
	@if [ -f "$(PLIST_DIR)/$(PLIST_FILE)" ]; then \
		launchctl bootout gui/$$(id -u) "$(PLIST_DIR)/$(PLIST_FILE)" 2>/dev/null || true; \
		rm -f "$(PLIST_DIR)/$(PLIST_FILE)"; \
		echo "Removed launchd service"; \
	fi
	@if [ -w "$(BIN_DIR)/$(BINARY)" ] || [ -w "$(BIN_DIR)" ]; then \
		rm -f "$(BIN_DIR)/$(BINARY)"; \
	else \
		sudo rm -f "$(BIN_DIR)/$(BINARY)"; \
	fi
else
	@if [ -f "$(UNIT_DIR)/$(UNIT_FILE)" ]; then \
		systemctl --user disable $(BINARY) 2>/dev/null || true; \
		rm -f "$(UNIT_DIR)/$(UNIT_FILE)"; \
		systemctl --user daemon-reload; \
		echo "Removed systemd service"; \
	fi
	rm -f "$(BIN_DIR)/$(BINARY)"
endif
	rm -f "$(SKILL_DIR)/$(SKILL_FILE)"
	@echo "Uninstalled $(BINARY)"

## service: Install and start the auto-start service
service:
ifeq ($(UNAME_S),Darwin)
	@mkdir -p $(PLIST_DIR)
	@if launchctl list com.agent-relay >/dev/null 2>&1; then \
		launchctl bootout gui/$$(id -u) "$(PLIST_DIR)/$(PLIST_FILE)" 2>/dev/null || true; \
	fi
	@printf '%s\n' \
		'<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0">' \
		'<dict>' \
		'    <key>Label</key>' \
		'    <string>com.agent-relay</string>' \
		'    <key>ProgramArguments</key>' \
		'    <array>' \
		'        <string>$(BIN_DIR)/$(BINARY)</string>' \
		'    </array>' \
		'    <key>RunAtLoad</key>' \
		'    <true/>' \
		'    <key>KeepAlive</key>' \
		'    <dict>' \
		'        <key>SuccessfulExit</key>' \
		'        <false/>' \
		'    </dict>' \
		'    <key>ThrottleInterval</key>' \
		'    <integer>5</integer>' \
		'    <key>ProcessType</key>' \
		'    <string>Background</string>' \
		'    <key>StandardOutPath</key>' \
		'    <string>/tmp/agent-relay.log</string>' \
		'    <key>StandardErrorPath</key>' \
		'    <string>/tmp/agent-relay.err</string>' \
		'</dict>' \
		'</plist>' > "$(PLIST_DIR)/$(PLIST_FILE)"
	@launchctl bootstrap gui/$$(id -u) "$(PLIST_DIR)/$(PLIST_FILE)" 2>/dev/null || \
		launchctl load "$(PLIST_DIR)/$(PLIST_FILE)" 2>/dev/null || true
	@echo "Started launchd service"
else
	@mkdir -p $(UNIT_DIR)
	@printf '[Unit]\nDescription=Claude Agentic Relay\nAfter=network.target\n\n[Service]\nType=simple\nExecStart=$(BIN_DIR)/$(BINARY)\nRestart=on-failure\nRestartSec=5\nEnvironment=PORT=8090\n\n[Install]\nWantedBy=default.target\n' > "$(UNIT_DIR)/$(UNIT_FILE)"
	@systemctl --user daemon-reload
	@systemctl --user enable $(BINARY) 2>/dev/null || true
	@systemctl --user restart $(BINARY)
	@echo "Started systemd service"
endif

## service-stop: Stop the running service
service-stop:
ifeq ($(UNAME_S),Darwin)
	@launchctl bootout gui/$$(id -u) "$(PLIST_DIR)/$(PLIST_FILE)" 2>/dev/null || true
else
	@systemctl --user stop $(BINARY) 2>/dev/null || true
endif

## skill: Install the /relay command
skill:
	@mkdir -p $(SKILL_DIR)
	cp skill/$(SKILL_FILE) $(SKILL_DIR)/$(SKILL_FILE)
	@echo "Installed /relay skill"

## clean: Remove build artifacts
clean:
	rm -f $(BINARY)

## help: Show available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | column -t -s ':'
