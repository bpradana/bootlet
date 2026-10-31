package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// The built website, copied in by `mise run relay:build`. Serving it from the
// relay puts the page and the relay on the same origin, which every browser
// allows (an HTTPS page reaching ws://localhost is blocked by Safari and
// needs a permission prompt in Chrome).
//
//go:embed all:site
var siteFS embed.FS

// embeddedSite returns the website built into the binary.
func embeddedSite() fs.FS {
	site, _ := fs.Sub(siteFS, "site")
	return site
}

// siteHandler serves a website, or explains how to get it when the binary
// was built without one.
func siteHandler(site fs.FS) http.Handler {
	if _, err := fs.Stat(site, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("This bootlet relay was built without the website.\n" +
				"Open https://bpradana.github.io/bootlet/ and press Connect in the network menu,\n" +
				"or build the relay with `mise run relay:build` to bundle the site.\n"))
		})
	}
	files := http.FileServerFS(site)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/relay.json" {
			// Tells the page to use this same origin as its relay.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"relay":"same-origin"}` + "\n"))
			return
		}
		files.ServeHTTP(w, r)
	})
}
