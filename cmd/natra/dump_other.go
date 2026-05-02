//go:build !linux

package main

import "errors"

func dumpStats([]string) error {
	return errors.New("dump-stats requires Linux")
}
