.PHONY: build install uninstall test run service-start service-plist service-stop service-status

build:
	go build -trimpath -buildvcs=false -o bin/herma ./cmd/herma

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
