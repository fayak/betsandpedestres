package resources

import "embed"

// FS exposes the static resource files.
//
//go:embed bogda.jpg
var FS embed.FS
