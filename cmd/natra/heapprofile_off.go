//go:build !heapprofile

package main

// maybeWriteHeapProfile is a no-op unless built with -tags heapprofile.
func maybeWriteHeapProfile(string) {}
