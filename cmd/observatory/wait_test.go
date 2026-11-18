package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func stateCheck(c *cli, want string) func(json.RawMessage, int) (bool, error) {
	return func(raw json.RawMessage, status int) (bool, error) {
		var v struct{ State string }
		json.Unmarshal(raw, &v)
		c.progress(v.State)
		return v.State == want, nil
	}
}

func TestWaitForDoneAndTimeout(t *testing.T) {
	h := newFakeHub(t)
	c, _, stderr := newTestCLI(t, h)
	c.terminal = true
	c.g.timeout = time.Second
	states := []string{"starting", "starting", "running"}
	n := 0
	h.onFunc("GET /p", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"state":"` + states[min(n, 2)] + `"}`))
		n++
	})
	raw, err := c.waitFor("vm dev", "/p", stateCheck(c, "running"))
	if err != nil || !strings.Contains(string(raw), "running") || len(h.calls("GET /p")) != 3 {
		t.Fatalf("done: %s %v %d", raw, err, len(h.calls("GET /p")))
	}
	if got := stderr.String(); got != "vm dev: starting\nvm dev: running\n" {
		t.Fatalf("progress: %q", got)
	}

	c, _, _ = newTestCLI(t, h)
	c.g.timeout = 20 * time.Millisecond
	start := time.Now()
	_, err = c.waitFor("vm dev", "/p", func(json.RawMessage, int) (bool, error) { return false, nil })
	if exitCode(err) != 6 || !strings.Contains(err.Error(), "timed out after 20ms") || time.Since(start) > time.Second {
		t.Fatalf("timeout: %v", err)
	}

	boom := errors.New("boom")
	_, err = c.waitFor("vm dev", "/p", func(json.RawMessage, int) (bool, error) { return false, boom })
	if !errors.Is(err, boom) || exitCode(err) != 1 {
		t.Fatalf("check error: %v", err)
	}
}

func TestWaitForPassesNotFound(t *testing.T) {
	h := newFakeHub(t)
	c, _, _ := newTestCLI(t, h)
	c.g.timeout = time.Second
	h.on("GET /gone", 404, `{"error":"not found"}`)
	_, err := c.waitFor("vm dev", "/gone", func(raw json.RawMessage, status int) (bool, error) {
		if raw != nil || status != 404 {
			t.Errorf("raw=%s status=%d", raw, status)
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h.on("GET /bad", 500, `{"error":"x"}`)
	_, err = c.waitFor("vm dev", "/bad", func(json.RawMessage, int) (bool, error) { t.Error("check called"); return true, nil })
	if exitCode(err) != 1 {
		t.Fatalf("500: %v", err)
	}
}
