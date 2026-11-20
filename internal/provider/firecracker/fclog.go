package firecracker

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"strings"
	"time"
)

// fcLogName is Firecracker's own log in the jail root; the guest's serial console stays in console.log, which never leaves the node.
const fcLogName = "firecracker.log"

// fcLogger is the logger config for both fc.json and the restore path; Info would log every API request, the balloon polls included.
var fcLogger = map[string]any{"log_path": fcLogName, "level": "Warning", "show_level": true, "show_log_origin": false}

var fcLevels = map[string]slog.Level{"TRACE": slog.LevelDebug, "DEBUG": slog.LevelDebug, "INFO": slog.LevelInfo, "WARN": slog.LevelWarn, "ERROR": slog.LevelError}

// parseFCLogLine splits "<time> [<id>:<thread>:<LEVEL>] <msg>"; other lines (e.g. panic output) are Info as a whole.
func parseFCLogLine(line string) (slog.Level, string) {
	_, rest, ok := strings.Cut(line, " [")
	if !ok {
		return slog.LevelInfo, line
	}
	tag, msg, ok := strings.Cut(rest, "] ")
	if !ok {
		return slog.LevelInfo, line
	}
	level, ok := fcLevels[tag[strings.LastIndexByte(tag, ':')+1:]]
	if !ok {
		return slog.LevelInfo, line
	}
	return level, msg
}

// tailFCLog logs each line of path until ctx is done, then reads once more for the lines written as the VM exited.
func tailFCLog(ctx context.Context, path, vmID, vmName string) {
	f, err := os.Open(path)
	if err != nil {
		slog.Warn("firecracker log", "vm", vmID, "err", err)
		return
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var partial string
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		for {
			s, err := r.ReadString('\n')
			partial += s
			if err != nil {
				break
			}
			level, msg := parseFCLogLine(strings.TrimSuffix(partial, "\n"))
			partial = ""
			// ctx only signals the VM's end; the last lines must still go out.
			slog.Log(context.Background(), level, msg, "source", "firecracker", "vm", vmID, "vm_name", vmName)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
}
