package cli

import (
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
)

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func randomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate secure random value: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func validateCredentials(credentials map[string]string) error {
	if len(credentials) == 0 {
		return errors.New("credentials must contain at least one identity; run herma init first")
	}
	seen := make(map[string]bool)
	for name, token := range credentials {
		if !identityPattern.MatchString(name) {
			return fmt.Errorf("invalid identity %q: use 1–64 letters, digits, dots, underscores or hyphens, starting with a letter or digit", name)
		}
		if len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
			return fmt.Errorf("identity %q requires a token of at least 32 characters without whitespace", name)
		}
		if seen[token] {
			return errors.New("every identity must have a distinct token")
		}
		seen[token] = true
	}
	return nil
}

func loadCredentials(path string) (map[string]string, error) {
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
	var credentials map[string]string
	if err := json.Unmarshal(data, &credentials); err != nil {
		return nil, errors.New("credentials must be a JSON object mapping identity names to tokens")
	}
	if err := validateCredentials(credentials); err != nil {
		return nil, err
	}
	return credentials, nil
}

func initCredentials(path string) error {
	token, err := randomID()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-init-*")
	if err != nil {
		return fmt.Errorf("prepare credentials: %w", err)
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(map[string]string{"owner": token}); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close credentials: %w", err)
	}
	// Linking the complete temporary file is atomic and fails if the destination
	// already exists, including a symlink. A racing server never sees partial JSON.
	if err := os.Link(f.Name(), path); err != nil {
		return fmt.Errorf("create credentials without overwriting an existing file: %w", err)
	}
	return nil
}

func addIdentity(path, name string) error {
	if !identityPattern.MatchString(name) {
		return fmt.Errorf("invalid identity %q: use 1–64 letters, digits, dots, underscores or hyphens, starting with a letter or digit", name)
	}
	lockPath, err := filepath.Abs(path + ".lock")
	if err != nil {
		return fmt.Errorf("resolve credentials lock path: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("credentials update lock already exists at %q; remove that file only after ensuring no herma identity add process is running, then retry: %w", lockPath, err)
		}
		return fmt.Errorf("lock credentials for update: %w", err)
	}
	defer func() { _ = lock.Close(); _ = os.Remove(lockPath) }()
	credentials, err := loadCredentials(path)
	if err != nil {
		return err
	}
	if _, ok := credentials[name]; ok {
		return fmt.Errorf("identity %q already exists", name)
	}
	token, err := randomID()
	if err != nil {
		return err
	}
	credentials[name] = token
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return fmt.Errorf("prepare credentials update: %w", err)
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(credentials); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace credentials: %w", err)
	}
	return nil
}
