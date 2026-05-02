//go:build !linux

// Stub for non-Linux platforms. The natra binary only ships for Linux,
// but `go test ./...` on macOS still needs pkg/bpf to compile so plain
// builds on developer machines don't error out. None of these stubs
// would ever execute in production — natra binary on Linux compiles
// loader.go instead.

package bpf

import "errors"

type Config struct {
	RateBps     uint64
	BurstBytes  uint64
	HHThreshold uint64
}

type TokenBucket struct {
	_lock        uint32
	_pad         uint32
	Tokens       uint64
	LastUpdateNs uint64
}

const (
	StatPassed    uint32 = 0
	StatThrottled uint32 = 1
	StatHHHits    uint32 = 2
)

type Stats struct {
	Passed    uint64
	Throttled uint64
	HHHits    uint64
}

type Program struct{}

var errNotLinux = errors.New("pkg/bpf: BPF requires Linux; the natra binary is Linux-only")

func Load() (*Program, error)            { return nil, errNotLinux }
func (*Program) Configure(Config) error  { return errNotLinux }
func (*Program) AttachIngress(int, string) error { return errNotLinux }
func (*Program) Stats() (Stats, error)   { return Stats{}, errNotLinux }
func (*Program) Close() error            { return nil }
