package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// sudo herma upgrade must not leave root-owned plugin files in the user's
// data folder: the plugin is written as the folder's owner.
func TestPluginIsWrittenAsTheDataFolderOwnerWhenRootUpgrades(t *testing.T) {
	for _, c := range []struct {
		name               string
		euid               int
		ownerUID, ownerGID uint32
		wantUID, wantGID   uint32
		switches           bool
	}{
		{"root upgrading a user's install", 0, 501, 20, 501, 20, true},
		{"a user upgrading their own install", 501, 501, 20, 0, 0, false},
		{"root upgrading root's own install", 0, 0, 0, 0, 0, false},
		// Only root can become someone else; anyone else writes as themselves.
		{"a user and another user's folder", 502, 501, 20, 0, 0, false},
	} {
		got := pluginWriter(c.euid, c.ownerUID, c.ownerGID)
		if (got != nil) != c.switches {
			t.Errorf("%s: switches identity %t, want %t", c.name, got != nil, c.switches)
			continue
		}
		if got != nil && (got.Uid != c.wantUID || got.Gid != c.wantGID) {
			t.Errorf("%s: writes as %d:%d, want %d:%d", c.name, got.Uid, got.Gid, c.wantUID, c.wantGID)
		}
	}
}

func TestFolderOwnerReadsTheOwnerOfAFolder(t *testing.T) {
	dir := t.TempDir()
	uid, _, ok := folderOwner(dir)
	if !ok || int(uid) != os.Getuid() {
		t.Errorf("owner of a folder this test made: %d (ok %t), want %d", uid, ok, os.Getuid())
	}
	if _, _, ok := folderOwner(filepath.Join(dir, "missing")); ok {
		t.Error("a folder that does not exist has an owner")
	}
}
