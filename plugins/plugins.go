// Package plugins carries the agent-client plugins built into herma, so an
// installed binary needs no checkout beside it.
package plugins

import (
	"embed"
	"io/fs"
)

//go:embed claude/.claude-plugin/plugin.json claude/hooks/hooks.json claude/hooks/*.ts
var files embed.FS

// Claude returns the Claude Code plugin's files, rooted at the plugin folder.
func Claude() fs.FS {
	sub, err := fs.Sub(files, "claude")
	if err != nil {
		panic(err) // the embedded tree always holds "claude"
	}
	return sub
}
