//go:build !linux

package main

import "errors"

func enterNetns(path string) (func(), error) {
	return nil, errors.New("netns operations require Linux")
}

func hostsidePeerIfIndex(netnsPath, ifName string) (int, error) {
	return 0, errors.New("hostside peer lookup requires Linux")
}

func installFQ(ifIndex int) error {
	return errors.New("fq qdisc install requires Linux")
}
