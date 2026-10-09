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
var ErrMalformedBlock = errors.New("herma block markers in AGENTS.md are malformed: keep exactly one start and one end marker, each on its own line")

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

// markerLines returns the [start, end) byte spans of the lines holding marker:
// the marker at the start of a line, followed by optional spaces or tabs and an
// optional "\r", then a newline or the end of the file. end includes the newline.
func markerLines(data []byte, marker string) [][2]int {
	var spans [][2]int
	for pos := 0; pos < len(data); {
		end := len(data)
		if nl := bytes.IndexByte(data[pos:], '\n'); nl >= 0 {
			end = pos + nl + 1
		}
		line := bytes.TrimSuffix(data[pos:end], []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		line = bytes.TrimRight(line, " \t")
		if string(line) == marker {
			spans = append(spans, [2]int{pos, end})
		}
		pos = end
	}
	return spans
}

// withBlock returns data with the herma block set to content, appended after
// a blank line when absent, or removed (with the blank line herma added) when
// content is empty. Markers are matched as whole lines.
func withBlock(data, content []byte) ([]byte, error) {
	starts, ends := markerLines(data, CodexStart), markerLines(data, CodexEnd)
	if bytes.Count(data, []byte(CodexStart)) != len(starts) || bytes.Count(data, []byte(CodexEnd)) != len(ends) ||
		len(starts) != len(ends) || len(starts) > 1 || (len(starts) == 1 && ends[0][0] < starts[0][1]) {
		return nil, ErrMalformedBlock
	}
	var block []byte
	if len(content) > 0 {
		block = append([]byte(CodexStart+"\n"), content...)
		if !bytes.HasSuffix(content, []byte("\n")) {
			block = append(block, '\n')
		}
		block = append(block, CodexEnd+"\n"...)
	}
	if len(starts) == 0 {
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
	before, after := data[:starts[0][0]], data[ends[0][1]:]
	if block == nil && bytes.HasSuffix(before, []byte("\n\n")) {
		before = before[:len(before)-1]
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
