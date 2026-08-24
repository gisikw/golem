package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/gisikw/golem/client"
	"github.com/gisikw/golem/contrib/familiar/nav"
	"github.com/gisikw/golem/protocol"
)

type source struct{ c *client.Client }

func (s source) List(ctx context.Context, state protocol.State) ([]protocol.Job, error) {
	return s.c.List(ctx, state)
}

func main() {
	endpoint := flag.String("service", env("GOLEM_ENDPOINT", "http://127.0.0.1:7337"), "Golem service URL")
	listen := flag.String("listen", env("GOLEM_NAV_LISTEN", "127.0.0.1:7340"), "loopback listen address")
	flag.Parse()
	live := func(ctx context.Context, t protocol.TerminalEndpoint) bool {
		return exec.CommandContext(ctx, "tmux", "-S", t.Socket, "has-session", "-t", t.Target).Run() == nil
	}
	s := nav.New(source{client.New(*endpoint)}, live, 25*time.Second)
	go s.RunRefresh(context.Background(), 250*time.Millisecond)
	log.Printf("golem Familiar nav listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, s.Handler()))
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
