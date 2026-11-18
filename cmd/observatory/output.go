package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func (c *cli) printJSON(raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := c.stdout.Write(buf.Bytes())
	return err
}

func (c *cli) emit(raw json.RawMessage, header []string, rows [][]string) error {
	if c.g.json {
		return c.printJSON(raw)
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(header, "\t")))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

func (c *cli) emitKV(raw json.RawMessage, pairs [][2]string) error {
	if c.g.json {
		return c.printJSON(raw)
	}
	w := 0
	for _, p := range pairs {
		w = max(w, len(p[0])+1)
	}
	for _, p := range pairs {
		if _, err := fmt.Fprintf(c.stdout, "%-*s %s\n", w, p[0]+":", p[1]); err != nil {
			return err
		}
	}
	return nil
}

func (c *cli) done(raw json.RawMessage, format string, a ...any) error {
	if c.g.json {
		if len(raw) == 0 {
			return nil
		}
		return c.printJSON(raw)
	}
	_, err := fmt.Fprintf(c.stdout, format+"\n", a...)
	return err
}

func ago(now, t time.Time) string {
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return t.In(now.Location()).Format("2006-01-02 15:04")
}

func gb(mb int) string {
	switch {
	case mb < 1024:
		return fmt.Sprintf("%d MB", mb)
	case mb%1024 == 0:
		return fmt.Sprintf("%d GB", mb/1024)
	}
	return strconv.FormatFloat(float64(mb)/1024, 'f', 1, 64) + " GB"
}
