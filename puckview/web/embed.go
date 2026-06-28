// Package web carries the embedded, zero-build-step dashboard (HTML/CSS/JS).
// Assets are baked into the binary so deployment is a single static file.
package web

import "embed"

//go:embed assets/index.html assets/style.css assets/app.js
var FS embed.FS
