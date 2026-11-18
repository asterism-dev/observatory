package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

func (c *cli) progress(state string) {
	if c.terminal && state != c.lastProgress {
		c.lastProgress = state
		fmt.Fprintf(c.stderr, "%s: %s\n", c.progressWhat, state)
	}
}

func (c *cli) waitFor(what, path string, check func(raw json.RawMessage, status int) (done bool, err error)) (json.RawMessage, error) {
	timeout := c.g.timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	c.progressWhat, c.lastProgress = what, ""
	for {
		var raw json.RawMessage
		status := http.StatusOK
		if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
			var ae *apiError
			if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
				return nil, err
			}
			raw, status = nil, ae.Status
		}
		done, err := check(raw, status)
		if err != nil || done {
			return raw, err
		}
		if !time.Now().Add(c.poll).Before(deadline) {
			return nil, &exitError{6, what + ": timed out after " + timeout.String()}
		}
		select {
		case <-time.After(c.poll):
		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		}
	}
}
