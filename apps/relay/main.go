// Command relay gives bootlet guests network access. It is meant to run on
// the user's own computer: it serves the bootlet website at
// http://127.0.0.1:8089 and, on the same port, a Wisp v1 endpoint through
// which the page's TCP streams reach the internet. By default it listens on
// loopback only, accepts pages from the official site and localhost, and
// only reaches public addresses on common ports.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8089", "listen address (use :8089 to accept other machines)")
	ports := flag.String("ports", "80,443,9418,22", "comma-separated destination ports to allow (\"all\" for any)")
	origins := flag.String("origins", "bpradana.github.io,localhost:*,127.0.0.1:*",
		"comma-separated page origins allowed to use the relay (\"*\" allows any); the relay's own pages are always allowed")
	serveSite := flag.Bool("site", true, "serve the bundled bootlet website")
	allowPrivate := flag.Bool("allow-private", false, "allow loopback, private and link-local destinations (local testing only)")
	maxStreams := flag.Int("max-streams", 64, "maximum concurrent streams per client")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := Config{AllowPrivate: *allowPrivate, MaxStreams: *maxStreams}
	if *ports != "all" {
		cfg.Ports = map[uint16]bool{}
		for _, p := range strings.Split(*ports, ",") {
			n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 16)
			if err != nil {
				log.Error("bad -ports value", "port", p)
				os.Exit(2)
			}
			cfg.Ports[uint16(n)] = true
		}
	}
	for _, o := range strings.Split(*origins, ",") {
		if o = strings.TrimSpace(o); o != "" && o != "*" {
			cfg.Origins = append(cfg.Origins, o)
		}
	}

	wisp := NewServer(cfg, log)
	site := siteHandler(embeddedSite())
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !*serveSite {
			wisp.ServeHTTP(w, r)
			return
		}
		site.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Info("bootlet relay listening", "addr", *addr, "ports", *ports, "allow_private", *allowPrivate)
	if *serveSite {
		host := *addr
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		fmt.Fprintf(os.Stderr, "\n  Open http://%s/ to use bootlet with networking,\n"+
			"  or press Connect in the network menu on https://bpradana.github.io/bootlet/ (Chrome, Edge).\n\n", host)
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
