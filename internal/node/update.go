package node

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/asterism-dev/observatory/internal/proto"
)

// Updater runs install.sh --update detached from the node service; a successful run restarts the service, so Wait rarely returns on the old binary.
type Updater interface {
	Start(ctx context.Context, hubBase string) error
	Wait(ctx context.Context) error
}

func hubBase(connectURL string) string {
	scheme, rest, _ := strings.Cut(connectURL, "://")
	base, _, _ := strings.Cut(rest, "/api/nodes/connect")
	return strings.Replace(scheme, "ws", "http", 1) + "://" + base
}

func (a *Agent) runUpdate(ctx context.Context, send func(proto.Message) error) {
	fail := func(err error) {
		slog.Error("update failed", "err", err)
		if err := send(proto.Message{Type: "update", Update: &proto.UpdateReport{Error: err.Error()}}); err != nil {
			slog.Warn("update report failed", "err", err)
		}
	}
	if a.Updater == nil {
		fail(errors.New("updates not supported on this node"))
		return
	}
	if !a.updating.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.updating.Store(false)
		err := a.Updater.Start(ctx, hubBase(a.HubURL))
		if err == nil {
			err = a.Updater.Wait(ctx)
		}
		if err != nil && ctx.Err() == nil {
			// No restart happened, so the next unrelated one must hibernate as usual.
			a.removeShutdownMarker()
			fail(err)
		}
	}()
}

func (a *Agent) removeShutdownMarker() {
	if a.ShutdownMarker == "" {
		return
	}
	if err := os.Remove(a.ShutdownMarker); err != nil && !os.IsNotExist(err) {
		slog.Error("shutdown marker failed", "err", err)
	}
}
