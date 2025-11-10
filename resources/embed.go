package resources

import "embed"

// FS exposes the static resource files.
//
//go:embed bogda.jpg favicon.png SpaceGrotesk-VariableFont_wght.ttf
var FS embed.FS
