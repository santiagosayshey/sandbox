// Command fetchassets downloads the front-end files the page embeds. They
// are not tracked in git; `go generate ./...` runs this before a build.
// Versions are pinned here so Renovate can bump them; unpkg serves a given
// version immutably, so the pin is what fixes the bytes.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// renovate: datasource=npm depName=htmx.org
const htmxVersion = "2.0.9"

// renovate: datasource=npm depName=htmx-ext-sse
const sseVersion = "2.2.4"

var assets = []struct{ url, name string }{
	{"https://unpkg.com/htmx.org@" + htmxVersion + "/dist/htmx.min.js", "htmx.min.js"},
	{"https://unpkg.com/htmx-ext-sse@" + sseVersion + "/sse.js", "sse.js"},
}

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	for _, a := range assets {
		if err := fetch(a.url, filepath.Join(dir, a.name)); err != nil {
			fmt.Fprintf(os.Stderr, "fetchassets: %s: %v\n", a.name, err)
			os.Exit(1)
		}
	}
}

func fetch(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return fmt.Errorf("%s: empty response", url)
	}
	return os.WriteFile(dest, b, 0o644)
}
