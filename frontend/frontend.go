// Package frontend holds the embedded static web assets for the runclaude
// session viewer. Keeping them in their own package with a single go:embed
// keeps the binary self-contained (no runtime file dependencies) while letting
// the serve package mount them as an http.FileSystem.
package frontend

import (
	"embed"
	"io/fs"
)

//go:embed index.html app.js style.css
var files embed.FS

// FS is the embedded asset tree (index.html, app.js, style.css).
var FS fs.FS = files
