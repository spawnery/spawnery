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

package render

import (
	"strings"
	"testing"
)

// Absent and zero are different answers and the difference is load-bearing:
// a group that says nothing must be refused, not defaulted to Paper's 20.
func TestValuesRejectsAnAbsentMaxPlayers(t *testing.T) {
	var v Values
	err := v.RequireMaxPlayers()
	if err == nil {
		t.Fatal("an absent maxPlayers was accepted")
	}
	if !strings.Contains(err.Error(), "maxPlayers") {
		t.Errorf("error = %q, want it to name the key", err)
	}
}

func TestValuesRejectsAZeroMaxPlayers(t *testing.T) {
	zero := int32(0)
	v := Values{MaxPlayers: &zero}
	if err := v.RequireMaxPlayers(); err == nil {
		t.Fatal("maxPlayers: 0 was accepted")
	}
}

func TestValuesAcceptsAPositiveMaxPlayers(t *testing.T) {
	n := int32(100)
	v := Values{MaxPlayers: &n}
	if err := v.RequireMaxPlayers(); err != nil {
		t.Fatalf("RequireMaxPlayers: %v", err)
	}
}

// RequirePlayerLimit guards the worse failure mode of the two: a zero limit
// does not just mean uncapped planning, it means internal/agent.Registry
// discards every player report the proxy ever sends because players will
// exceed slots. That silent metric-reads-zero case gets the same three
// cases as RequireMaxPlayers so a regression here is caught the same way.
func TestValuesRejectsAnAbsentPlayerLimit(t *testing.T) {
	var v Values
	err := v.RequirePlayerLimit()
	if err == nil {
		t.Fatal("an absent playerLimit was accepted")
	}
	if !strings.Contains(err.Error(), "playerLimit") {
		t.Errorf("error = %q, want it to name the key", err)
	}
}

func TestValuesRejectsAZeroPlayerLimit(t *testing.T) {
	zero := int32(0)
	v := Values{PlayerLimit: &zero}
	if err := v.RequirePlayerLimit(); err == nil {
		t.Fatal("playerLimit: 0 was accepted")
	}
}

func TestValuesAcceptsAPositivePlayerLimit(t *testing.T) {
	n := int32(100)
	v := Values{PlayerLimit: &n}
	if err := v.RequirePlayerLimit(); err != nil {
		t.Fatalf("RequirePlayerLimit: %v", err)
	}
}

// onlineMode has no zero to reject — false is a legitimate answer, and the
// point of the refusal is that there is no safe direction to guess in. So the
// three cases are absent, false and true, and the two present ones must both
// be accepted rather than one of them quietly treated as unset.
func TestValuesRejectsAnAbsentOnlineMode(t *testing.T) {
	var v Values
	err := v.RequireOnlineMode()
	if err == nil {
		t.Fatal("an absent onlineMode was accepted")
	}
	if !strings.Contains(err.Error(), "onlineMode") {
		t.Errorf("error = %q, want it to name the key", err)
	}
}

func TestValuesAcceptsOnlineModeEitherWay(t *testing.T) {
	for _, want := range []bool{false, true} {
		value := want
		v := Values{OnlineMode: &value}
		if err := v.RequireOnlineMode(); err != nil {
			t.Errorf("RequireOnlineMode with onlineMode: %v: %v", want, err)
		}
	}
}
