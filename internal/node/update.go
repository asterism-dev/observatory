package node

import (
	"context"
	"errors"
	"log"
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
	host, _, _ := strings.Cut(rest, "/")
	return strings.Replace(scheme, "ws", "http", 1) + "://" + host
}

func (a *Agent) runUpdate(ctx context.Context, send func(proto.Message) error) {
	fail := func(err error) {
		log.Printf("update: %v", err)
		if err := send(proto.Message{Type: "update", Update: &proto.UpdateReport{Error: err.Error()}}); err != nil {
			log.Printf("update report: %v", err)
		}
	}
	if a.Updater == nil {
		fail(errors.New("updates not supported on this node"))
		return
	}
	if !a.updating.CompareAndSwap(false, true) {
		return
	}
	defer a.updating.Store(false)
	err := a.Updater.Start(ctx, hubBase(a.HubURL))
	if err == nil {
		err = a.Updater.Wait(ctx)
	}
	if err != nil && ctx.Err() == nil {
		fail(err)
	}
}
