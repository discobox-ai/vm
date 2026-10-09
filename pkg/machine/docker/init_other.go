//go:build !linux

package docker

import "errors"

// Init runs inside a docker guest, which is always linux.
func Init() error { return errors.New("docker-init runs inside a linux container") }

// Shift runs inside a docker install container, which is always linux.
func Shift() error { return errors.New("docker-init --shift runs inside a linux container") }
