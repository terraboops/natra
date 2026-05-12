//go:build !linux

package main

import "errors"

func profileCmd(args []string) error {
	return errors.New("natra profile requires Linux (BPF program stats are kernel-only)")
}
