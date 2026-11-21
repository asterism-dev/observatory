// Command hub is a stopgap until `observatory serve hub` replaces it.
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/asterism-dev/observatory/internal/hubserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	addr := os.Getenv("OBSERVATORY_LISTEN")
	if addr == "" {
		addr = ":8090"
	}
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		err = hubserver.Run(ctx, os.Getenv, ln)
	}
	if err != nil {
		slog.Error("hub failed", "err", err)
		os.Exit(1)
	}
}
