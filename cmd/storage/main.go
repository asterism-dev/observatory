//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/storage"
	"github.com/asterism-dev/observatory/internal/version"
)

func main() {
	hubURL := flag.String("hub", "http://127.0.0.1:8090", "hub base URL")
	dir := flag.String("dir", "/disks", "disk file directory")
	keyDir := flag.String("key-dir", "/run/storage", "export key directory")
	addr := flag.String("addr", netaddr.GatewayUnderlay.String(), "address the exports listen on")
	flag.Parse()
	log.Printf("%s %s", filepath.Base(os.Args[0]), version.Version)

	token := os.Getenv("OBSERVATORY_STORAGE_TOKEN")
	if token == "" {
		log.Fatal("OBSERVATORY_STORAGE_TOKEN is not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := storage.Dir{Path: *dir}
	ex := &storage.Exports{Addr: *addr, KeyDir: *keyDir, Dir: d, Spawn: spawn}
	hub := storage.Hub{URL: *hubURL, Token: token, HTTP: &http.Client{Timeout: 5 * time.Second}}
	// Exiting kills every nbdkit through Pdeathsig.
	storage.Run(ctx, hub, d, ex, 2*time.Second)
}

func spawn(args []string) (func(), <-chan error, error) {
	cmd := exec.Command("nbdkit", args...)
	var tail lastLine
	cmd.Stdout = os.Stderr
	cmd.Stderr = io.MultiWriter(os.Stderr, &tail)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	started, done, exited := make(chan error, 1), make(chan error, 1), make(chan struct{})
	go func() {
		// Pdeathsig fires when the starting thread exits, so this goroutine keeps its thread until nbdkit is gone.
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		err := cmd.Wait()
		if line := tail.String(); err != nil && line != "" {
			err = fmt.Errorf("%w: %s", err, line)
		}
		done <- err
		close(exited)
	}()
	if err := <-started; err != nil {
		return nil, nil, err
	}
	kill := func() {
		// Never SIGTERM: a graceful nbdkit shutdown fails the client's in-flight writes instead of letting it reconnect.
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			log.Printf("kill nbdkit: %v", err)
		}
		<-exited
	}
	return kill, done, nil
}

// lastLine keeps the last non-empty line written to it, to report why nbdkit exited.
type lastLine struct {
	mu   sync.Mutex
	line []byte
}

func (l *lastLine) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range bytes.Split(p, []byte("\n")) {
		if s = bytes.TrimSpace(s); len(s) > 0 {
			l.line = append(l.line[:0], s...)
		}
	}
	return len(p), nil
}

func (l *lastLine) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.line)
}
