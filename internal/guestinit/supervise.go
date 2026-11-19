package guestinit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	serviceLog      = "/var/log/observatory/cmd.log"
	serviceLogLimit = 10 << 20
	maxBackoff      = time.Minute
	stopGrace       = 10 * time.Second
)

// Service runs the image's Entrypoint+Cmd, restarting it with backoff whenever it exits.
type Service struct {
	Argv, Env  []string
	Dir        string
	StopSignal string
	Log        string

	pid   atomic.Int64
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

// Run blocks until ctx is done, then stops the service with StopSignal and SIGKILL after 10 s.
func (s *Service) Run(ctx context.Context) {
	if s.now == nil {
		s.now, s.after = time.Now, time.After
	}
	log := &rotatingLog{path: s.Log, limit: serviceLogLimit}
	defer log.Close()
	backoff := time.Second
	for {
		start := s.now()
		s.runOnce(ctx, log)
		if ctx.Err() != nil {
			return
		}
		if s.now().Sub(start) >= maxBackoff {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-s.after(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (s *Service) runOnce(ctx context.Context, log *rotatingLog) {
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = s.Env, s.Dir, log, log
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(log, "observatory: start %s: %v\n", s.Argv[0], err)
		return
	}
	s.pid.Store(int64(cmd.Process.Pid))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = cmd.Process.Signal(parseSignal(s.StopSignal))
		select {
		case err = <-done:
		case <-s.after(stopGrace):
			_ = cmd.Process.Kill()
			err = <-done
		}
	}
	if err == nil {
		fmt.Fprintf(log, "observatory: %s exited\n", s.Argv[0])
		return
	}
	fmt.Fprintf(log, "observatory: %s exited: %v\n", s.Argv[0], err)
}

// parseSignal accepts OCI StopSignal forms ("SIGINT", "INT", "2"); anything unknown means SIGTERM.
func parseSignal(name string) syscall.Signal {
	if n, err := strconv.Atoi(name); err == nil && n > 0 {
		return syscall.Signal(n)
	}
	name = strings.ToUpper(name)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	if sig := unix.SignalNum(name); sig != 0 {
		return sig
	}
	return syscall.SIGTERM
}

// rotatingLog keeps one previous file (.1); exec.Cmd calls Write from one goroutine at a time since Stdout == Stderr.
type rotatingLog struct {
	path  string
	limit int64
	f     *os.File
	size  int64
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	if l.f != nil && l.size > 0 && l.size+int64(len(p)) > l.limit {
		l.f.Close()
		l.f = nil
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return 0, err
		}
	}
	if l.f == nil {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
		if err != nil {
			return 0, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, err
		}
		l.f, l.size = f, fi.Size()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *rotatingLog) Close() error {
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}
