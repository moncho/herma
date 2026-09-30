.PHONY: build test run service-start service-stop service-status

build:
	go build -trimpath -buildvcs=false -o bin/herma ./cmd/herma

test:
	go test -race ./...
	go vet ./...

run:
	go run -buildvcs=false ./cmd/herma serve

# macOS background service for the current login session. Absolute paths keep
# the service independent of whichever directory launchd starts it from.
# Optional: make service-start BACKUP_DIR=/path/to/synced/folder
BACKUP_FLAGS = $(if $(BACKUP_DIR),--backup-dir "$(BACKUP_DIR)")

service-start: build
	@test -f "$(CURDIR)/.herma/credentials.json" || { echo "Run ./bin/herma init first." >&2; exit 1; }
	launchctl submit -l local.herma -o "$(CURDIR)/.herma/server.log" -e "$(CURDIR)/.herma/server.log" -- "$(CURDIR)/bin/herma" --credentials "$(CURDIR)/.herma/credentials.json" serve --db "$(CURDIR)/.herma/knowledge.sqlite3" --listen 127.0.0.1:8765 $(BACKUP_FLAGS)

service-stop:
	launchctl remove local.herma

service-status:
	launchctl list local.herma
