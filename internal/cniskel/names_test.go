package cniskel

import (
	"regexp"
	"testing"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/utils"
)

// upstreamName is the regexp cni/pkg/utils uses. Tests may link
// regexp; only the plugin binary must not.
var upstreamName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]*$`)

var nameCases = []string{
	"", "a", "A", "0", "_", ".", "-", "a_", "a.", "a-", "_a", ".a", "-a",
	"abc123", "3f9a1c0e2b7d", "k8s_POD_default-1.2", "a b", "a/b", "a:b",
	"a\x00", "é", "aé", "a\n", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
}

func TestValidNameMatchesUpstreamRegexp(t *testing.T) {
	for _, s := range nameCases {
		want := s == "" || upstreamName.MatchString(s)
		if got := validName(s); got != want {
			t.Errorf("validName(%q) = %v, upstream regexp says %v", s, got, want)
		}
	}
}

// The three validators must return exactly what cni/pkg/utils does,
// code and message included.
func TestValidatorsMatchUpstream(t *testing.T) {
	ifNames := append([]string{"eth0", "..", "0123456789abcdef", "a\tb", "ok:no"}, nameCases...)
	for _, s := range nameCases {
		assertSameError(t, "containerID", s, validateContainerID(s), utils.ValidateContainerID(s))
		assertSameError(t, "networkName", s, validateNetworkName(s), utils.ValidateNetworkName(s))
	}
	for _, s := range ifNames {
		assertSameError(t, "ifName", s, validateInterfaceName(s), utils.ValidateInterfaceName(s))
	}
}

func assertSameError(t *testing.T, what, in string, got, want *types.Error) {
	t.Helper()
	if (got == nil) != (want == nil) || (got != nil && *got != *want) {
		t.Errorf("%s(%q): got %v, upstream %v", what, in, got, want)
	}
}

func FuzzValidNameMatchesUpstreamRegexp(f *testing.F) {
	for _, s := range nameCases {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		want := s == "" || upstreamName.MatchString(s)
		if got := validName(s); got != want {
			t.Fatalf("validName(%q) = %v, upstream regexp says %v", s, got, want)
		}
	})
}
