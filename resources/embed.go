package resources

import "embed"

// FS exposes the static resource files.
//
//go:embed bogda.jpg favicon.png
var FS embed.FS
