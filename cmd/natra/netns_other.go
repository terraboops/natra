//go:build !linux

package main

import "errors"

func enterNetns(path string) (func(), error) {
	return nil, errors.New("netns operations require Linux")
}
