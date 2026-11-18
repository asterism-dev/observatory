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

	"github.com/asterism-dev/observatory/internal/egress"
	"github.com/asterism-dev/observatory/internal/proto"
)

const maxTableLine = 4 << 20

// runEgress serves one VM's proxy; every stdin line is a full table, EOF stops it.
func runEgress(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("egress", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "listen address, host:port")
	vm := fs.String("vm", "", "VM id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *listen == "" {
		return errors.New("egress: -listen is required")
	}
	if err := harden(); err != nil {
		return fmt.Errorf("egress: harden: %w", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	p := egress.New(*vm, log.New(stderr, "egress: ", log.LstdFlags))

	serveErr := make(chan error, 1)
	stdinErr := make(chan error, 1)
	go func() { stdinErr <- readTables(stdin, p, stdout, func() { go func() { serveErr <- p.Serve(ln) }() }) }()
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
