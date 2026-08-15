/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"strconv"
	"strings"
)

// PersistentServerName is the name of the server holding one ordinal of a
// persistent group.
//
// The ordinal is the identity of a persistent server, and this name is how it
// is carried: podspec.DataClaimName derives the claim from the server's name,
// so this string is also what makes a world addressable across every deletion
// and recreation of the Server object. An ephemeral server is named by
// NewServerName instead, with a random suffix, because it has no identity to
// preserve.
func PersistentServerName(group string, ordinal int32) string {
	return group + "-" + strconv.Itoa(int(ordinal))
}

// OrdinalOf reads the ordinal back out of a server name, and reports whether
// the name is one PersistentServerName could have produced for this group.
//
// The boundary is the last hyphen and the prefix must equal the group exactly,
// which is what keeps a group whose own name ends in a number from reading its
// own name as an ordinal. The digits are parsed strictly: "survival-01" is
// refused rather than read as 1, because no name this package writes looks like
// that and accepting it would let two strings claim one identity.
func OrdinalOf(group, server string) (int32, bool) {
	prefix := group + "-"
	if !strings.HasPrefix(server, prefix) {
		return 0, false
	}
	digits := server[len(prefix):]
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 31)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}
