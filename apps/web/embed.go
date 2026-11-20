//go:build embedweb

// Package web embeds the built web UI. Build it with `pnpm build` before `go build -tags embedweb`.
package web

import "embed"

// Dist is apps/web/dist.
//
//go:embed all:dist
var Dist embed.FS
