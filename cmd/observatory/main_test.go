package main

import (
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestParseIntersperse(t *testing.T) {
	c, _, _ := newTestCLI(t, nil)
	fs := c.flags("vm delete")
	wait := fs.Bool("wait", false, "")
	pos, err := c.parse(fs, []string{"dev", "--yes", "--wait"}, 1, 1)
	if err != nil || !slices.Equal(pos, []string{"dev"}) || !c.g.yes || !*wait {
		t.Fatalf("pos %v err %v yes %v wait %v", pos, err, c.g.yes, *wait)
	}

	fs = c.flags("ssh")
	pos, err = c.parse(fs, []string{"dev", "--", "-L", "1:2"}, 1, -1)
	if err != nil || !slices.Equal(pos, []string{"dev", "-L", "1:2"}) {
		t.Fatalf("pos %v err %v", pos, err)
	}

	for _, args := range [][]string{nil, {"a", "b"}} {
		_, err := c.parse(c.flags("vm delete"), args, 1, 1)
		if exitCode(err) != 2 {
			t.Errorf("%v: exit %d (%v), want 2", args, exitCode(err), err)
		}
	}
}

func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{usagef("bad %s", "x"), 2},
		{&apiError{Status: 401}, 3},
		{&apiError{Status: 403, Msg: "forbidden"}, 4},
		{&apiError{Status: 404, Msg: "not found"}, 5},
		{notFoundf("vm %q not found", "x"), 5},
		{&exitError{6, "timed out"}, 6},
		{&apiError{Status: 500, Msg: "internal error"}, 1},
		{errors.New("boom"), 1},
		{fmt.Errorf("wrapped: %w", &apiError{Status: 403}), 4},
	} {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	if msg := (&apiError{Status: http.StatusUnauthorized, Msg: "unauthorized"}).Error(); !strings.Contains(msg, "observatory login") {
		t.Errorf("401 message %q lacks login hint", msg)
	}
	if err := exec.Command("sh", "-c", "exit 7").Run(); exitCode(err) != 7 {
		t.Errorf("exec exit code: %d", exitCode(err))
	}
}

func TestHelpGolden(t *testing.T) {
	c, stdout, _ := newTestCLI(t, nil)
	if code := run(c, []string{"help"}); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	for _, cmd := range commands() {
		if !strings.Contains(stdout.String(), cmd.path) {
			t.Errorf("help lacks %q", cmd.path)
		}
	}
	golden(t, "help.golden", stdout.String())

	c, _, stderr := newTestCLI(t, nil)
	if code := run(c, []string{"nope"}); code != 2 {
		t.Fatalf("unknown command exit %d", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "nope"`) {
		t.Errorf("stderr %q", stderr.String())
	}
	c, _, _ = newTestCLI(t, nil)
	if code := run(c, nil); code != 2 {
		t.Errorf("no command exit %d, want 2", code)
	}
	c, stdout, _ = newTestCLI(t, nil)
	if code := run(c, []string{"org", "--help"}); code != 0 || !strings.Contains(stdout.String(), "org audit") || strings.Contains(stdout.String(), "whoami") {
		t.Errorf("org --help exit %d, stdout %q", code, stdout.String())
	}
}
