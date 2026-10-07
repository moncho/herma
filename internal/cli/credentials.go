package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/moncho/herma/internal/api"
	"github.com/moncho/herma/internal/store"
)

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// credential is one identity's bearer token and role. The file maps identity
// names to these entries.
type credential struct {
	Token string     `json:"token"`
	Role  store.Role `json:"role"`
}

func randomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate secure random value: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func validateCredentials(credentials map[string]credential) error {
	if len(credentials) == 0 {
		return errors.New("credentials must contain at least one identity; run herma init first")
	}
	seen := make(map[string]bool)
	for name, entry := range credentials {
		if !identityPattern.MatchString(name) {
			return fmt.Errorf("invalid identity %q: use 1–64 letters, digits, dots, underscores or hyphens, starting with a letter or digit", name)
		}
		if len(entry.Token) < 32 || strings.ContainsAny(entry.Token, " \t\r\n") {
			return fmt.Errorf("identity %q requires a token of at least 32 characters without whitespace", name)
		}
		if !entry.Role.Valid() {
			return fmt.Errorf("identity %q has role %q; use reviewer, agent, or read-only", name, entry.Role)
		}
		if seen[entry.Token] {
			return errors.New("every identity must have a distinct token")
		}
		seen[entry.Token] = true
	}
	return nil
}

// loadServerIdentities loads every credentials file herma serve reads and merges
// them. A single file need not hold a reviewer, but the merged set must, so a
// reviewer's token can live apart from the file agents use.
func loadServerIdentities(paths []string) (map[string]credential, error) {
	merged := make(map[string]credential)
	tokens := make(map[string]bool)
	for _, path := range paths {
		credentials, err := loadCredentials(path)
		if err != nil {
			return nil, err
		}
		for name, entry := range credentials {
			if _, ok := merged[name]; ok {
				return nil, fmt.Errorf("identity %q appears in more than one credentials file", name)
			}
			if tokens[entry.Token] {
				return nil, errors.New("every identity must have a distinct token, across all credentials files")
			}
			tokens[entry.Token] = true
			merged[name] = entry
		}
	}
	if !hasReviewer(merged) {
		return nil, errors.New("herma serve needs at least one reviewer identity; add one to --credentials or pass --reviewer-credentials")
	}
	return merged, nil
}

func loadCredentials(path string) (map[string]credential, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read credentials %s (run herma init if needed): %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("credentials must be a regular file, not a symlink")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("credentials file %s is accessible to others; change its permissions to 0600", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open credentials: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, errors.New("credentials file exceeds 1 MiB")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errors.New(`credentials must be a JSON object mapping identity names to {"token", "role"} entries`)
	}
	credentials := make(map[string]credential, len(raw))
	for name, value := range raw {
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.DisallowUnknownFields()
		var entry credential
		if err := decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("identity %q must be an object with only token and role", name)
		}
		credentials[name] = entry
	}
	if err := validateCredentials(credentials); err != nil {
		return nil, err
	}
	return credentials, nil
}

// stageCredentials writes a complete private credentials file next to path and
// returns its name. The caller publishes it atomically and removes it after.
func stageCredentials(path string, credentials map[string]credential) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return "", fmt.Errorf("prepare credentials: %w", err)
	}
	name := f.Name()
	staged := false
	defer func() {
		_ = f.Close()
		if !staged {
			_ = os.Remove(name)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return "", err
	}
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(credentials); err != nil {
		return "", fmt.Errorf("write credentials: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("save credentials: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close credentials: %w", err)
	}
	staged = true
	return name, nil
}

func initCredentials(path string) error {
	ownerToken, err := randomID()
	if err != nil {
		return err
	}
	agentToken, err := randomID()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	return createCredentials(path, map[string]credential{
		"owner":       {Token: ownerToken, Role: store.RoleReviewer},
		"local-agent": {Token: agentToken, Role: store.RoleAgent},
	})
}

// createCredentials writes a new private credentials file at path and fails if
// anything already exists there.
func createCredentials(path string, credentials map[string]credential) error {
	staged, err := stageCredentials(path, credentials)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	// Linking the complete temporary file is atomic and fails if the destination
	// already exists, including a symlink. A racing server never sees partial JSON.
	if err := os.Link(staged, path); err != nil {
		return fmt.Errorf("create credentials without overwriting an existing file: %w", err)
	}
	return nil
}

// updateCredentials applies change under the update lock, validates the
// result and replaces the file atomically.
func updateCredentials(path string, change func(map[string]credential) error) error {
	lockPath, err := filepath.Abs(path + ".lock")
	if err != nil {
		return fmt.Errorf("resolve credentials lock path: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("credentials update lock already exists at %q; remove that file only after ensuring no herma identity command is running, then retry: %w", lockPath, err)
		}
		return fmt.Errorf("lock credentials for update: %w", err)
	}
	defer func() { _ = lock.Close(); _ = os.Remove(lockPath) }()
	credentials, err := loadCredentials(path)
	if err != nil {
		return err
	}
	if err := change(credentials); err != nil {
		return err
	}
	if err := validateCredentials(credentials); err != nil {
		return err
	}
	staged, err := stageCredentials(path, credentials)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	if err := os.Rename(staged, path); err != nil {
		return fmt.Errorf("replace credentials: %w", err)
	}
	return nil
}

// addIdentity adds name to the file at path. With clientFile set, it also
// writes clientFile holding only the new identity, for a client on another
// machine; the identity is added only if that file is written.
func addIdentity(path, name string, role store.Role, clientFile string) error {
	if !identityPattern.MatchString(name) {
		return fmt.Errorf("invalid identity %q: use 1–64 letters, digits, dots, underscores or hyphens, starting with a letter or digit", name)
	}
	if !role.Valid() {
		return fmt.Errorf("role %q is not valid; use reviewer, agent, or read-only", role)
	}
	if clientFile != "" && role == store.RoleReviewer {
		return errors.New("a reviewer cannot use a client file: reviewer tokens work only on the server's local socket")
	}
	wrote := false
	err := updateCredentials(path, func(credentials map[string]credential) error {
		if _, ok := credentials[name]; ok {
			return fmt.Errorf("identity %q already exists", name)
		}
		token, err := randomID()
		if err != nil {
			return err
		}
		entry := credential{Token: token, Role: role}
		if clientFile != "" {
			if err := createCredentials(clientFile, map[string]credential{name: entry}); err != nil {
				return fmt.Errorf("client file: %w", err)
			}
			wrote = true
		}
		credentials[name] = entry
		return nil
	})
	if err != nil && wrote {
		_ = os.Remove(clientFile)
	}
	return err
}

// revokeIdentity removes name from the file at path. It reports whether that
// removed the file's last reviewer; herma serve rejects such a change unless
// another credentials file it reads still holds a reviewer.
func revokeIdentity(path, name string) (bool, error) {
	lastReviewer := false
	err := updateCredentials(path, func(credentials map[string]credential) error {
		entry, ok := credentials[name]
		if !ok {
			return fmt.Errorf("identity %q is not in the credentials file", name)
		}
		delete(credentials, name)
		lastReviewer = entry.Role == store.RoleReviewer && !hasReviewer(credentials)
		return nil
	})
	return lastReviewer, err
}

func hasReviewer(credentials map[string]credential) bool {
	for _, entry := range credentials {
		if entry.Role == store.RoleReviewer {
			return true
		}
	}
	return false
}

func apiIdentities(credentials map[string]credential) []api.Identity {
	identities := make([]api.Identity, 0, len(credentials))
	for name, entry := range credentials {
		identities = append(identities, api.Identity{Name: name, Token: entry.Token, Role: entry.Role})
	}
	return identities
}
