// Copyright 2019 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Name validators from github.com/containernetworking/cni/pkg/utils
// v1.3.1. validName replaces utils' regexp
// `^[a-zA-Z0-9][a-zA-Z0-9_.\-]*$` with a byte loop; messages and
// error codes are unchanged.

package cniskel

import (
	"bytes"
	"fmt"
	"unicode"

	"github.com/containernetworking/cni/pkg/types"
)

// maxInterfaceNameLength is the length max of a valid interface name
const maxInterfaceNameLength = 15

// validName matches `^[a-zA-Z0-9][a-zA-Z0-9_.\-]*$`. Like the regexp,
// it accepts the empty string; callers reject that separately.
func validName(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '_' || c == '.' || c == '-') {
			continue
		}
		return false
	}
	return true
}

// validateContainerID will validate that the supplied containerID is not empty does not contain invalid characters
func validateContainerID(containerID string) *types.Error {
	if containerID == "" {
		return types.NewError(types.ErrUnknownContainer, "missing containerID", "")
	}
	if !validName(containerID) {
		return types.NewError(types.ErrInvalidEnvironmentVariables, "invalid characters in containerID", containerID)
	}
	return nil
}

// validateNetworkName will validate that the supplied networkName does not contain invalid characters
func validateNetworkName(networkName string) *types.Error {
	if networkName == "" {
		return types.NewError(types.ErrInvalidNetworkConfig, "missing network name:", "")
	}
	if !validName(networkName) {
		return types.NewError(types.ErrInvalidNetworkConfig, "invalid characters found in network name", networkName)
	}
	return nil
}

// validateInterfaceName will validate the interface name based on the four rules below
// 1. The name must not be empty
// 2. The name must be less than 16 characters
// 3. The name must not be "." or ".."
// 4. The name must not contain / or : or any whitespace characters
// ref to https://github.com/torvalds/linux/blob/master/net/core/dev.c#L1024
func validateInterfaceName(ifName string) *types.Error {
	if len(ifName) == 0 {
		return types.NewError(types.ErrInvalidEnvironmentVariables, "interface name is empty", "")
	}
	if len(ifName) > maxInterfaceNameLength {
		return types.NewError(types.ErrInvalidEnvironmentVariables, "interface name is too long", fmt.Sprintf("interface name should be less than %d characters", maxInterfaceNameLength+1))
	}
	if ifName == "." || ifName == ".." {
		return types.NewError(types.ErrInvalidEnvironmentVariables, "interface name is . or ..", "")
	}
	for _, r := range bytes.Runes([]byte(ifName)) {
		if r == '/' || r == ':' || unicode.IsSpace(r) {
			return types.NewError(types.ErrInvalidEnvironmentVariables, "interface name contains / or : or whitespace characters", "")
		}
	}

	return nil
}
