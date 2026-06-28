// Command puckview is a boutique Wake-on-LAN + LAN-presence + box-diagnostics
// dashboard for pucks. Single static binary, tailnet-only, quiet by default.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zw00sh/puck/puckview/internal/config"
	"github.com/zw00sh/puck/puckview/internal/netconf"
	"github.com/zw00sh/puck/puckview/internal/server"
	"github.com/zw00sh/puck/puckview/internal/store"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("puckview: ")

	cfg := config.Load()

	ni, err := netconf.Detect(cfg.Iface)
	if err != nil {
		log.Printf("warning: interface auto-detect failed: %v (diagnostics will be limited)", err)
	} else {
		log.Printf("LAN iface %s ip=%s cidr=%s bcast=%s gw=%s", ni.Iface, ni.IP, ni.CIDR, ni.Broadcast, ni.Gateway)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store %s: %v", cfg.DBPath, err)
	}
	defer st.Close()

	srv := server.New(cfg, ni, st, version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go srv.Run(ctx)

	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("listening on http://%s (version %s)", cfg.Listen, version)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	log.Print("shut down cleanly")
}
