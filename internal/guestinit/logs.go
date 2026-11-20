package guestinit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

const maxLogLine = 16 << 10

var (
	stderr io.Writer = os.Stderr
	// telemetry is the init's sink to the node, nil until Run starts it.
	telemetry *Sink
)

type level struct {
	num  logspb.SeverityNumber
	text string
}

var (
	debugLevel = level{logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, "DEBUG"}
	infoLevel  = level{logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO"}
	warnLevel  = level{logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "WARN"}
	errorLevel = level{logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "ERROR"}
)

// logf writes an init message to the console until the node's sink is connected, then only to stream system, so it is not counted twice.
func logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if telemetry.Connected() {
		telemetry.Logs("system", []*logspb.LogRecord{logRecord(time.Now(), msg, infoLevel)})
		return
	}
	fmt.Fprintln(stderr, msg)
}

// consolef is logf for messages right before reboot(2) or a kernel panic, which the queue may never deliver.
func consolef(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(stderr, msg)
	if telemetry.Connected() {
		telemetry.Logs("system", []*logspb.LogRecord{logRecord(time.Now(), msg, infoLevel)})
	}
}

func logRecord(t time.Time, msg string, lvl level, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	return &logspb.LogRecord{
		TimeUnixNano: uint64(t.UnixNano()), ObservedTimeUnixNano: uint64(time.Now().UnixNano()),
		SeverityNumber: lvl.num, SeverityText: lvl.text,
		Body:       stringValue(msg[:min(len(msg), maxLogLine)]),
		Attributes: attrs,
	}
}

// stringValue drops invalid UTF-8, which protobuf refuses to encode.
func stringValue(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strings.ToValidUTF8(s, "")}}
}

// followServiceLog sends the image's output to stream service: under systemd its journal, read by a journalctl that startInImage starts, else what nsinit writes to cmd.log from cmdLogStart on.
func followServiceLog(ctx context.Context, sink *Sink, systemd bool, startInImage func(*exec.Cmd) error, cmdLogStart int64) {
	emit := func(lines []string) {
		now := time.Now()
		recs := make([]*logspb.LogRecord, len(lines))
		for i, l := range lines {
			recs[i] = logRecord(now, l, infoLevel)
		}
		sink.Logs("service", recs)
	}
	if !systemd {
		followFile(ctx, serviceLog, cmdLogStart, time.Second, emit)
		return
	}
	bin := journalctl()
	if bin == "" {
		logf("init: service log: the image has no journalctl")
		return
	}
	followJournal(ctx, bin, startInImage, func(recs []*logspb.LogRecord) { sink.Logs("service", recs) })
}

// followFile emits the lines appended to path from offset on, every poll, following a rename (rotation) and truncation of path until ctx ends.
func followFile(ctx context.Context, path string, offset int64, poll time.Duration, emit func([]string)) {
	var f *os.File
	var partial []byte
	skipping := false
	buf := make([]byte, 64<<10)
	drain := func() {
		var lines []string
		for {
			n, err := f.Read(buf)
			offset += int64(n)
			for chunk := buf[:n]; len(chunk) > 0; {
				i := bytes.IndexByte(chunk, '\n')
				if i < 0 {
					if !skipping {
						partial = append(partial, chunk...)
					}
					chunk = nil
				} else {
					if !skipping {
						l := append(partial, chunk[:i]...)
						lines = append(lines, string(l[:min(len(l), maxLogLine)]))
					}
					partial, skipping, chunk = partial[:0], false, chunk[i+1:]
				}
				if len(partial) > maxLogLine {
					lines = append(lines, string(partial[:maxLogLine]))
					partial, skipping = partial[:0], true
				}
			}
			if len(lines) >= maxRequestRecords || (err != nil && len(lines) > 0) {
				emit(lines)
				lines = nil
			}
			if err != nil {
				return
			}
		}
	}
	for {
		if f == nil {
			if nf, err := os.Open(path); err == nil {
				f = nf
				if fi, err := f.Stat(); err != nil || fi.Size() < offset {
					offset = 0
				}
				f.Seek(offset, io.SeekStart)
			}
		}
		if f != nil {
			drain()
			cur, cerr := f.Stat()
			next, nerr := os.Stat(path)
			switch {
			case cerr == nil && nerr == nil && !os.SameFile(cur, next):
				// Rotated: what was written before the rename is read before moving on.
				drain()
				f.Close()
				f, offset = nil, 0
				continue
			case cerr == nil && cur.Size() < offset:
				f.Seek(0, io.SeekStart)
				offset, partial, skipping = 0, partial[:0], false
			}
		}
		select {
		case <-ctx.Done():
			if f != nil {
				f.Close()
			}
			return
		case <-time.After(poll):
		}
	}
}

// journalctl returns the image's journalctl, or "".
func journalctl() string {
	for _, p := range []string{"/usr/bin/journalctl", "/bin/journalctl"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// followJournal runs journalctl through start and emits its entries, restarting it after the last cursor with backoff whenever it exits.
func followJournal(ctx context.Context, bin string, start func(*exec.Cmd) error, emit func([]*logspb.LogRecord)) {
	cursor := ""
	backoff := time.Second
	for ctx.Err() == nil {
		args := []string{"-f", "-o", "json", "--all", "--no-tail", "-b"}
		if cursor != "" {
			args = []string{"-f", "-o", "json", "--all", "--after-cursor=" + cursor}
		}
		started := time.Now()
		cmd := exec.CommandContext(ctx, bin, args...)
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = start(cmd)
		}
		if err == nil {
			cursor = readJournal(out, cursor, emit)
			err = cmd.Wait()
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logf("init: service log: journalctl: %v", err)
		}
		if time.Since(started) >= time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// readJournal emits entries in batches of what is already buffered and returns the last cursor seen.
func readJournal(r io.Reader, cursor string, emit func([]*logspb.LogRecord)) string {
	br := bufio.NewReaderSize(r, 1<<20)
	var batch []*logspb.LogRecord
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// An entry over 1 MiB is skipped rather than buffered.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			continue
		}
		if len(line) > 0 {
			rec, c, ok := journalRecord(line)
			if c != "" {
				cursor = c
			}
			if ok {
				batch = append(batch, rec)
			}
		}
		if len(batch) > 0 && (err != nil || br.Buffered() == 0 || len(batch) >= maxRequestRecords) {
			emit(batch)
			batch = nil
		}
		if err != nil {
			return cursor
		}
	}
}

// journalRecord maps one journalctl -o json entry; kernel messages are skipped (ok false) as the console stream has them.
func journalRecord(line []byte) (rec *logspb.LogRecord, cursor string, ok bool) {
	var e map[string]json.RawMessage
	if json.Unmarshal(line, &e) != nil {
		return nil, "", false
	}
	field := func(k string) string {
		var s string
		json.Unmarshal(e[k], &s)
		return s
	}
	cursor = field("__CURSOR")
	if field("_TRANSPORT") == "kernel" {
		return nil, cursor, false
	}
	msg := field("MESSAGE")
	// journalctl writes a message that is not valid UTF-8 as an array of bytes.
	var raw []byte
	if msg == "" && json.Unmarshal(e["MESSAGE"], &raw) == nil {
		msg = string(raw)
	}
	t := time.Now()
	if us, err := strconv.ParseInt(field("__REALTIME_TIMESTAMP"), 10, 64); err == nil {
		t = time.UnixMicro(us)
	}
	lvl := infoLevel
	switch field("PRIORITY") {
	case "0", "1", "2", "3":
		lvl = errorLevel
	case "4":
		lvl = warnLevel
	case "7":
		lvl = debugLevel
	}
	var attrs []*commonpb.KeyValue
	if unit := field("_SYSTEMD_UNIT"); unit != "" {
		attrs = append(attrs, &commonpb.KeyValue{Key: "unit", Value: stringValue(unit)})
	}
	return logRecord(t, msg, lvl, attrs...), cursor, true
}
