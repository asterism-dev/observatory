//go:build !linux

package main

import "errors"

func powerOff() error { return errors.New("poweroff is only supported on linux") }
