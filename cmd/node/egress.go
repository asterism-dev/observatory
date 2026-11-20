package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/asterism-dev/observatory/internal/egress"
	"github.com/asterism-dev/observatory/internal/proto"
)

const maxTableLine = 4 << 20

const usageEvery = 30 * time.Second

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// usageReporter returns a Report that never blocks: it queues for one writer of `usage <json>` lines, which throttles readings per account and drops them when the queue is full.
func usageReporter(w io.Writer) func(proto.UsageObservation) {
	// Limits get their own queue, read first and never throttled, so a burst of readings cannot crowd one out.
	readings := make(chan proto.UsageObservation, 64)
	limits := make(chan proto.UsageObservation, 64)
	go func() {
		last := map[string]time.Time{}
		for {
			var o proto.UsageObservation
			select {
			case o = <-limits:
			default:
				select {
				case o = <-limits:
				case o = <-readings:
					if time.Since(last[o.Account]) < usageEvery {
						continue
					}
				}
			}
			last[o.Account] = time.Now()
			b, err := json.Marshal(o)
			if err != nil {
				continue
			}
			if _, err := w.Write(append(append([]byte("usage "), b...), '\n')); err != nil {
				return
			}
		}
	}()
	return func(o proto.UsageObservation) {
		q := readings
		if o.LimitedUntil != nil {
			q = limits
		}
		select {
		case q <- o:
		default:
		}
	}
}

func egressMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// net/http logs raw upstream bytes through the global logger, and the node copies stderr into the journal.
	log.SetOutput(io.Discard)
	if err := runEgress(args, stdin, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// runEgress serves one VM's proxy; every stdin line is a full table, EOF stops it.
func runEgress(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("egress", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "listen address, host:port")
	vm := fs.String("vm", "", "VM id")
	peer := fs.String("peer", "", "the only client address served (the VM's)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *listen == "" {
		return errors.New("egress: -listen is required")
	}
	var peerAddr netip.Addr
	if *peer != "" {
		var err error
		if peerAddr, err = netip.ParseAddr(*peer); err != nil {
			return fmt.Errorf("egress: -peer: %w", err)
		}
	}
	if err := harden(); err != nil {
		return fmt.Errorf("egress: harden: %w", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	// The node prefixes and timestamps each stderr line.
	p := egress.New(*vm, log.New(stderr, "", 0))
	p.Peer = peerAddr
	out := &lockedWriter{w: stdout}
	p.Report = usageReporter(out)

	serveErr := make(chan error, 1)
	stdinErr := make(chan error, 1)
	go func() { stdinErr <- readTables(stdin, p, out, func() { go func() { serveErr <- p.Serve(ln) }() }) }()
	select {
	case err := <-stdinErr:
		return err
	case err := <-serveErr:
		return err
	}
}

func readTables(stdin io.Reader, p *egress.Proxy, stdout io.Writer, start func()) error {
	// Fixed-size buffer: the scanner never reallocates, so the token can be zeroed after use.
	buf := make([]byte, maxTableLine)
	sc := bufio.NewScanner(stdin)
	sc.Buffer(buf, maxTableLine)
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		var t proto.Egress
		err := json.Unmarshal(line, &t)
		clear(line)
		if err != nil {
			return errors.New("egress: invalid table")
		}
		err = p.SetTable(t)
		t = proto.Egress{}
		if err != nil {
			return errors.New("egress: table rejected")
		}
		if first {
			first = false
			start()
			if _, err := io.WriteString(stdout, "ready\n"); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return errors.New("egress: table line too long")
		}
		return err
	}
	return nil
}
