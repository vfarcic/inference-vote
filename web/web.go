// Package web holds the pages, embedded so the binary is the whole app.
package web

import "embed"

//go:embed index.html presenter.html app.js style.css
var Files embed.FS
