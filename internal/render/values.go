/*
Copyright paul_wtf.

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

// Package render turns the operator's rendered configuration into the files
// Paper and Velocity read.
package render

import "fmt"

// Values is the neutral document the operator renders into a ConfigMap. Fields
// are pointers because absent and zero are different answers.
type Values struct {
	// MaxPlayers is reported by the Paper agent as slots, which the operator
	// scales on.
	MaxPlayers  *int32  `yaml:"maxPlayers,omitempty" json:"maxPlayers,omitempty"`
	PlayerLimit *int32  `yaml:"playerLimit,omitempty" json:"playerLimit,omitempty"`
	Motd        *string `yaml:"motd,omitempty" json:"motd,omitempty"`
	// OnlineMode is the proxy's setting; a Paper server is always
	// online-mode=false.
	OnlineMode *bool `yaml:"onlineMode,omitempty" json:"onlineMode,omitempty"`
	// AcceptsTransfers omits false, so a group without transfer keeps its
	// config.yaml and pod hash.
	AcceptsTransfers bool `yaml:"acceptsTransfers,omitempty" json:"acceptsTransfers,omitempty"`
}

// RequireMaxPlayers refuses a backend without its capacity: the upstream
// default of 20 would have the operator plan against slots the server
// cannot honour.
func (v Values) RequireMaxPlayers() error {
	return requirePositive("maxPlayers", v.MaxPlayers)
}

// RequirePlayerLimit refuses a zero limit, which would make the registry
// discard every player count the proxy sends.
func (v Values) RequirePlayerLimit() error {
	return requirePositive("playerLimit", v.PlayerLimit)
}

// RequireOnlineMode refuses to guess in either direction: true would lock out
// a deliberately offline network, false would open one to any name. The
// operator always writes the key, so nil means a config.yaml from elsewhere.
func (v Values) RequireOnlineMode() error {
	if v.OnlineMode == nil {
		return fmt.Errorf("config.yaml: onlineMode is not set")
	}
	return nil
}

func requirePositive(key string, n *int32) error {
	if n == nil {
		return fmt.Errorf("config.yaml: %s is not set", key)
	}
	if *n <= 0 {
		return fmt.Errorf("config.yaml: %s is %d, want a positive number", key, *n)
	}
	return nil
}
