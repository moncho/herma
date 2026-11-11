.PHONY: build install uninstall test run release service-start service-plist service-stop service-status

# The version comes from the nearest tag (v0.1.0, v0.1.0-3-gabc1234, or
# -dirty); override it with: make build VERSION=v0.2.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/moncho/herma/internal/version.Version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

build:
	go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/herma ./cmd/herma

# Cross-compiles one archive per platform into dist/ with a checksum file.
# The SQLite driver is pure Go, so no C toolchain is needed.
release:
	rm -rf dist && mkdir -p dist
	for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/herma_$${os}_$${arch}/herma ./cmd/herma || exit 1; \
		tar -C dist/herma_$${os}_$${arch} -czf dist/herma_$(VERSION)_$${os}_$${arch}.tar.gz herma; \
		rm -r dist/herma_$${os}_$${arch}; \
	done
	cd dist && shasum -a 256 *.tar.gz > SHA256SUMS

# The binary carries the Claude Code plugin, so a plain copy is enough.
# Override the target folder with: make install BINDIR=~/.local/bin
BINDIR ?= /usr/local/bin

install: build
	install -d "$(BINDIR)"
	install -m 0755 bin/herma "$(BINDIR)/herma"

uninstall:
	rm -f "$(BINDIR)/herma"

test:
	go test -race ./...
	go vet ./...

run:
	go run -buildvcs=false ./cmd/herma serve

# macOS background service: a launch agent that runs the installed binary on
# herma's data folder (credentials, database, socket, log) and starts again
# at every login, so it survives a reboot.
# Optional: make service-start BACKUP_DIR=/path/to/synced/folder
#           REVIEWER_CREDENTIALS=/path/to/reviewer.json HERMA_DIR=/path/to/data
HERMA_DIR ?= $(HOME)/.config/herma
PLIST = $(HOME)/Library/LaunchAgents/local.herma.plist
BACKUP_FLAGS = $(if $(BACKUP_DIR),--backup-dir "$(BACKUP_DIR)")
REVIEWER_FLAGS = $(if $(REVIEWER_CREDENTIALS),--reviewer-credentials "$(REVIEWER_CREDENTIALS)")
SERVE_ARGS = "$(BINDIR)/herma" --credentials "$(HERMA_DIR)/credentials.json" serve --db "$(HERMA_DIR)/knowledge.sqlite3" --listen 127.0.0.1:8765 $(BACKUP_FLAGS) $(REVIEWER_FLAGS)

service-start: install service-plist
	-launchctl bootout gui/$$(id -u)/local.herma 2>/dev/null
	launchctl bootstrap gui/$$(id -u) "$(PLIST)"

service-plist:
	@test -f "$(HERMA_DIR)/credentials.json" || { echo "Run herma init first (credentials go to $(HERMA_DIR))." >&2; exit 1; }
	install -d "$(dir $(PLIST))"
	{ printf '%s\n' '<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0">' '<dict>' \
		'<key>Label</key><string>local.herma</string>' \
		'<key>ProgramArguments</key>' '<array>'; \
	  for arg in $(SERVE_ARGS); do printf '<string>%s</string>\n' "$$arg"; done; \
	  printf '%s\n' '</array>' \
		'<key>RunAtLoad</key><true/>' '<key>KeepAlive</key><true/>' \
		'<key>StandardOutPath</key><string>$(HERMA_DIR)/server.log</string>' \
		'<key>StandardErrorPath</key><string>$(HERMA_DIR)/server.log</string>' \
		'</dict>' '</plist>'; } > "$(PLIST)"
	plutil -lint "$(PLIST)"

service-stop:
	launchctl bootout gui/$$(id -u)/local.herma
	rm -f "$(PLIST)"

service-status:
	launchctl list local.herma
