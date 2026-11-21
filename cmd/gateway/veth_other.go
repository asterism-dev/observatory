//go:build !linux

package main

import "errors"

func removeHostVeths(string) error { return errors.New("removing host veths needs Linux") }
