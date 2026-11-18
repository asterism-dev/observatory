package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxSecret = 64 << 10

func (c *cli) confirm(question string) error {
	if c.g.yes {
		return nil
	}
	if !c.terminal {
		return usagef("%s: pass --yes to confirm", question)
	}
	fmt.Fprint(c.stderr, question+" [y/N] ")
	line, _ := bufio.NewReader(c.stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return nil
	}
	return &exitError{1, "aborted"}
}

// secret never includes the value in an error.
func (c *cli) secret(prompt, file string) (string, error) {
	var b []byte
	var err error
	switch {
	case file != "" && file != "-":
		var f *os.File
		if f, err = os.Open(file); err != nil {
			return "", errors.New("cannot read secret file: " + errors.Unwrap(err).Error())
		}
		defer f.Close()
		b, err = readLimited(f)
	case file == "" && c.terminal:
		fmt.Fprint(c.stderr, prompt)
		b, err = c.readPassword()
		fmt.Fprintln(c.stderr)
		if err == nil && len(b) > maxSecret {
			err = errTooLarge
		}
	default:
		b, err = readLimited(c.stdin)
	}
	if err != nil {
		return "", err
	}
	v := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if v == "" {
		return "", usagef("empty value")
	}
	return v, nil
}

var errTooLarge = errors.New("value larger than 64 KiB")

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxSecret+1))
	if err != nil {
		return nil, errors.New("cannot read secret")
	}
	// A trailing newline is trimmed afterwards, so allow it on top of the limit.
	if len(b) > maxSecret && !(len(b) == maxSecret+1 && b[maxSecret] == '\n') {
		return nil, errTooLarge
	}
	return b, nil
}
