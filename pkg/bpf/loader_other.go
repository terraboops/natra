//go:build !linux

// Stub for non-Linux platforms. The natra binary only ships for Linux,
// but `go test ./...` on macOS needs pkg/bpf to compile so dev builds
// don't error out. Nothing here ever runs in production.

package bpf

import "errors"

// AttachMode mirrors the Linux build's enum.
type AttachMode int

const (
	AttachTCX AttachMode = iota
	AttachClsactPodside
)

type Config struct {
	RateBps     uint64
	BurstBytes  uint64
	HHThreshold uint64
}

type TokenBucket struct {
	_            uint32 // bpf_spin_lock
	_            uint32 // alignment
	Tokens       uint64
	LastUpdateNs uint64
}

const (
	StatPassed    uint32 = 0
	StatThrottled uint32 = 1
	StatHHHits    uint32 = 2
)

type Program struct{}

var errNotLinux = errors.New("pkg/bpf: BPF requires Linux; the natra binary is Linux-only")

func Load() (*Program, error)                                { return nil, errNotLinux }
func (*Program) Configure(Config) error                      { return errNotLinux }
func (*Program) AttachIngress(int, AttachMode, string) error { return errNotLinux }
func (*Program) PinMaps(string, string) error                { return errNotLinux }
func (*Program) Close() error                                { return nil }
