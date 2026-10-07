.PHONY: build install uninstall test run service-start service-stop service-status

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

# macOS background service for the current login session, running the
# installed binary on herma's data folder (credentials, database, socket, log).
# Optional: make service-start BACKUP_DIR=/path/to/synced/folder
#           REVIEWER_CREDENTIALS=/path/to/reviewer.json HERMA_DIR=/path/to/data
HERMA_DIR ?= $(HOME)/.config/herma
BACKUP_FLAGS = $(if $(BACKUP_DIR),--backup-dir "$(BACKUP_DIR)")
REVIEWER_FLAGS = $(if $(REVIEWER_CREDENTIALS),--reviewer-credentials "$(REVIEWER_CREDENTIALS)")

service-start: install
	@test -f "$(HERMA_DIR)/credentials.json" || { echo "Run herma init first (credentials go to $(HERMA_DIR))." >&2; exit 1; }
	launchctl submit -l local.herma -o "$(HERMA_DIR)/server.log" -e "$(HERMA_DIR)/server.log" -- "$(BINDIR)/herma" --credentials "$(HERMA_DIR)/credentials.json" serve --db "$(HERMA_DIR)/knowledge.sqlite3" --listen 127.0.0.1:8765 $(BACKUP_FLAGS) $(REVIEWER_FLAGS)

service-stop:
	launchctl remove local.herma

service-status:
	launchctl list local.herma
