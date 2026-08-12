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

// Package render turns the operator's rendered configuration into the files
// Paper and Velocity actually read.
//
// It exists as a package rather than as code inside cmd/ for the same reason
// internal/slp does: the interesting part is a pure function of its inputs,
// and a table test is a far better way to find out whether online-mode came
// out right than starting a container.
package render

import "fmt"

// Values is the neutral document the operator renders into a ConfigMap. It is
// deliberately neither target's dialect: the operator stays out of the
// business of TOML and YAML, and adding a field later is one CRD field and one
// line in a flavour.
//
// Every field is a pointer because absent and zero are different answers, and
// the difference decides whether a server starts. See RequireMaxPlayers.
type Values struct {
	// MaxPlayers is a backend's player capacity. The Paper agent reports it to
	// the operator as slots, and the operator scales on that number.
	MaxPlayers *int32 `yaml:"maxPlayers,omitempty" json:"maxPlayers,omitempty"`
	// PlayerLimit is a proxy's player capacity.
	PlayerLimit *int32 `yaml:"playerLimit,omitempty" json:"playerLimit,omitempty"`
	// Motd is what a player sees in the server list.
	Motd *string `yaml:"motd,omitempty" json:"motd,omitempty"`
	// OnlineMode is whether a proxy authenticates players with Mojang. It is
	// the proxy's own setting and has no backend counterpart here: a Paper
	// server rendered by this package is always online-mode=false, because the
	// proxy in front of it is what authenticates. See RequireOnlineMode.
	OnlineMode *bool `yaml:"onlineMode,omitempty" json:"onlineMode,omitempty"`
}

// RequireMaxPlayers refuses a backend that does not know its own capacity.
//
// Starting with the upstream default of 20 while the group promises 100 makes
// the operator plan against capacity the server can never honour: it will keep
// sending players to a server that is already full. This refusal lived in
// image/entrypoint.sh against an environment variable until milestone 3b; it
// moved here rather than disappearing.
func (v Values) RequireMaxPlayers() error {
	return requirePositive("maxPlayers", v.MaxPlayers)
}

// RequirePlayerLimit refuses a proxy that does not know its own capacity. The
// agent reports it as slots, and internal/agent.Registry.ReportPlayers rejects
// any report where players exceed slots — so a zero limit would not just be
// unset capacity, it would make the proxy silently discard every player count
// it ever sends: a metric that reads zero while players are connected.
func (v Values) RequirePlayerLimit() error {
	return requirePositive("playerLimit", v.PlayerLimit)
}

// RequireOnlineMode refuses a proxy that does not say whether it authenticates
// players.
//
// Guessing is what this package will not do, and this is the field where
// guessing is least acceptable in either direction. Defaulting to true would
// override an operator who deliberately set false and produce a proxy nobody
// can join with an offline client, with nothing on the object saying why.
// Defaulting to false would silently open the whole network to anyone claiming
// any name — the exact failure ProxyGroup.spec.config.onlineMode exists to keep
// visible. The operator always writes the key (see
// internal/controller.proxyConfigValues), and the CRD defaults it to true, so
// the only way to arrive here nil is a config.yaml written by something other
// than this operator; that is worth a refusal that names the key.
func (v Values) RequireOnlineMode() error {
	if v.OnlineMode == nil {
		return fmt.Errorf("config.yaml: onlineMode is not set")
	}
	return nil
}

// requirePositive is the check RequireMaxPlayers and RequirePlayerLimit share.
// The two callers differ only in which field and key name they refuse on; the
// reason each refusal matters belongs on the exported method, not here.
func requirePositive(key string, n *int32) error {
	if n == nil {
		return fmt.Errorf("config.yaml: %s is not set", key)
	}
	if *n <= 0 {
		return fmt.Errorf("config.yaml: %s is %d, want a positive number", key, *n)
	}
	return nil
}
