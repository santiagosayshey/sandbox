// Package web holds the embedded page template and static assets.
//
// htmx.min.js and sse.js are not tracked; `go generate` fetches the pinned
// versions (see tools/fetchassets).
package web

import "embed"

//go:generate go run ../../tools/fetchassets .

// Assets are the files served under /static and the page template.
//
//go:embed index.html style.css htmx.min.js sse.js
var Assets embed.FS
