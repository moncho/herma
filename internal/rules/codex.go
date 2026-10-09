package rules

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Codex reads one global instruction file, AGENTS.md in its home folder, and
// has no include mechanism, so herma owns a delimited block inside it.
const (
	CodexStart = "<!-- herma:principles:start — generated; edit principles in herma -->"
	CodexEnd   = "<!-- herma:principles:end -->"
)

// ErrMalformedBlock reports herma markers that are unpaired, repeated or out
// of order; the file is left for the user to fix.
var ErrMalformedBlock = errors.New("herma block markers in AGENTS.md are malformed")

// CodexHome returns $CODEX_HOME, or ~/.codex.
func CodexHome() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// SyncCodexBlock makes the herma block in codexHome/AGENTS.md hold content
// and reports whether the file changed. Text outside the block is kept byte
// for byte. Empty content removes the block. A missing codexHome is a no-op.
func SyncCodexBlock(codexHome string, content []byte) (bool, error) {
	info, err := os.Lstat(codexHome)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%w: %s", ErrUnsafePath, codexHome)
	}
	root, err := os.OpenRoot(codexHome)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	current, mode, err := readCodexFile(root)
	if err != nil {
		return false, err
	}
	next, err := withBlock(current, content)
	if err != nil {
		return false, err
	}
	if bytes.Equal(current, next) {
		return false, nil
	}
	return true, writeCodexFile(root, next, mode)
}

func readCodexFile(root *os.Root) ([]byte, fs.FileMode, error) {
	info, err := root.Lstat("AGENTS.md")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0o644, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%w: AGENTS.md", ErrUnsafePath)
	}
	data, err := root.ReadFile("AGENTS.md")
	return data, info.Mode().Perm(), err
}

// withBlock returns data with the herma block set to content, appended after
// a blank line when absent, or removed (with the blank line herma added) when
// content is empty.
func withBlock(data, content []byte) ([]byte, error) {
	start := []byte(CodexStart + "\n")
	end := []byte(CodexEnd + "\n")
	si, ei := bytes.Index(data, start), bytes.Index(data, end)
	if bytes.Count(data, []byte(CodexStart)) > 1 || bytes.Count(data, []byte(CodexEnd)) > 1 || (si < 0) != (ei < 0) || (si >= 0 && ei < si) {
		return nil, ErrMalformedBlock
	}
	var block []byte
	if len(content) > 0 {
		block = append(append([]byte{}, start...), content...)
		if !bytes.HasSuffix(content, []byte("\n")) {
			block = append(block, '\n')
		}
		block = append(block, end...)
	}
	if si < 0 {
		if block == nil {
			return data, nil
		}
		if len(data) == 0 {
			return block, nil
		}
		sep := []byte("\n\n")
		if bytes.HasSuffix(data, []byte("\n")) {
			sep = []byte("\n")
		}
		return append(append(append([]byte{}, data...), sep...), block...), nil
	}
	before, after := data[:si], data[ei+len(end):]
	if block == nil {
		before = bytes.TrimSuffix(before, []byte("\n"))
		return append(append([]byte{}, before...), after...), nil
	}
	return append(append(append([]byte{}, before...), block...), after...), nil
}

func writeCodexFile(root *os.Root, data []byte, mode fs.FileMode) error {
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return err
	}
	tmp := ".AGENTS-" + hex.EncodeToString(entropy[:]) + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, "AGENTS.md")
	}
	if err != nil {
		_ = root.Remove(tmp)
	}
	return err
}
