//go:build !linux

package guestinit

import "errors"

func Run() error { return errors.New("the guest init runs on linux only") }
