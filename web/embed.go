// Package web embeds the admin UI (index.html, app.js, app.css).
//
// The files are served by the admin listener: index.html at /admin and
// app.js / app.css under /assets/. There is no build step.
package web

import "embed"

// FS holds the UI files at its root: "index.html", "app.js", "app.css".
//
//go:embed index.html app.js app.css
var FS embed.FS
