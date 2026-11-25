// Package webui serves the embedded larkbox inspector.
package webui

import "embed"

//go:embed all:dist
var bundle embed.FS
