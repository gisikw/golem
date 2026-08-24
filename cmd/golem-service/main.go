package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gisikw/golem/service"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	db := flag.String("db", env("GOLEM_DB", "agents.db"), "SQLite database path")
	socket := flag.String("unix", env("GOLEM_SOCKET", "agents.sock"), "Unix socket; empty disables")
	listen := flag.String("listen", env("GOLEM_LISTEN", "127.0.0.1:7337"), "loopback HTTP address; empty disables")
	flag.Parse()
	store, err := service.Open(*db)
	if err != nil {
		slog.Error("open store", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	h := service.API{Store: store}.Handler()
	servers := []*http.Server{}
	listeners := []net.Listener{}
	start := func(network, address string) error {
		if network == "unix" {
			if !filepath.IsAbs(address) {
				var e error
				address, e = filepath.Abs(address)
				if e != nil {
					return e
				}
			}
			if err := os.MkdirAll(filepath.Dir(address), 0o700); err != nil {
				return err
			}
			if fi, e := os.Lstat(address); e == nil {
				if fi.Mode()&os.ModeSocket == 0 {
					return errors.New("refusing to replace non-socket unix path")
				}
				_ = os.Remove(address)
			}
		}
		if network == "tcp" {
			host, _, e := net.SplitHostPort(address)
			if e != nil {
				return e
			}
			ip := net.ParseIP(host)
			if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
				return errors.New("TCP listen must be loopback")
			}
		}
		ln, e := net.Listen(network, address)
		if e != nil {
			return e
		}
		if network == "unix" {
			_ = os.Chmod(address, 0o600)
		}
		s := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
		servers = append(servers, s)
		listeners = append(listeners, ln)
		go func() {
			slog.Info("listening", "component", "agent-service", "network", network, "address", address)
			if e := s.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
				slog.Error("serve", "error", e)
			}
		}()
		return nil
	}
	if *socket != "" {
		if err = start("unix", *socket); err != nil {
			slog.Error("listen", "error", err)
			os.Exit(1)
		}
	}
	if *listen != "" {
		if err = start("tcp", *listen); err != nil {
			slog.Error("listen", "error", err)
			os.Exit(1)
		}
	}
	if len(servers) == 0 {
		slog.Error("no listeners configured")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	<-ctx.Done()
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
	for _, l := range listeners {
		_ = l.Close()
	}
}
