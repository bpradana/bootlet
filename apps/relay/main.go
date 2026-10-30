// Command relay is a Wisp v1 server that gives bootlet guests network access:
// the browser sends TCP streams over one WebSocket and the relay opens the
// real connections. By default it only reaches public addresses on common
// ports; see README.md before exposing it to the internet.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":8089", "listen address")
	ports := flag.String("ports", "80,443,9418,22", "comma-separated destination ports to allow (\"all\" for any)")
	origins := flag.String("origins", "", "comma-separated allowed page origins, e.g. bpradana.github.io (empty allows any)")
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
		if o = strings.TrimSpace(o); o != "" {
			cfg.Origins = append(cfg.Origins, o)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("/", NewServer(cfg, log))
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Info("wisp relay listening", "addr", *addr, "ports", *ports, "allow_private", *allowPrivate)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
