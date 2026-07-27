// matrix-nctalk - A Matrix–Nextcloud Talk puppeting bridge.
// Copyright (C) 2026 Don O'Neill
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	_ "embed"
	"strings"
	"time"

	"go.mau.fi/util/configupgrade"
	"go.mau.fi/util/random"
)

//go:embed example-config.yaml
var ExampleConfig string

// Config is the `network:` section of the bridge config.
type Config struct {
	// BotSecret is the shared secret for a bridge serving one Nextcloud server.
	BotSecret string `yaml:"bot_secret"`
	// BotSecrets holds a secret per Nextcloud host, for a bridge serving more
	// than one. Setting it disables BotSecret entirely: a single secret shared
	// between servers lets each of them forge webhooks for the others, since
	// the header naming the sending server is not covered by the signature.
	BotSecrets map[string]string `yaml:"bot_secrets"`
	BotName    string            `yaml:"bot_name"`

	AutoEnableBot bool `yaml:"auto_enable_bot"`

	AllowedServers []string `yaml:"allowed_servers"`

	RelayUnlinkedUsers bool `yaml:"relay_unlinked_users"`

	// CredentialKey encrypts the Nextcloud app passwords held in the bridge
	// database. Generated on first run; keep it and back it up with the
	// database, since losing it means every user has to log in again.
	CredentialKey string `yaml:"credential_key"`

	LoginTimeout time.Duration `yaml:"login_timeout"`

	// SyncInterval is how often bridged conversations are resynced. Zero uses
	// defaultSyncInterval; a negative value turns the resync off.
	SyncInterval time.Duration `yaml:"sync_interval"`
}

// BotSecretFor returns the shared secret to verify webhooks from a Nextcloud
// host with, or "" when the bridge has none and the request must be rejected.
//
// Selecting the secret by host is what makes the sending server's identity
// verifiable at all. The header naming it is not covered by the signature, so a
// single secret shared across servers would let any of them — or anyone who
// captured one signed request — claim to be another.
func (c *Config) BotSecretFor(host string) string {
	if len(c.BotSecrets) > 0 {
		for configured, secret := range c.BotSecrets {
			if strings.EqualFold(strings.TrimSpace(configured), host) {
				return secret
			}
		}
		// Deliberately no fall back to BotSecret: once per-host secrets exist,
		// honouring a global one for unlisted hosts would reopen the hole they
		// were configured to close.
		return ""
	}
	// With one secret there is only one server that holds it, so the secret is
	// the whole of the trust boundary. Where the operator has named their
	// servers, though, honouring that here too costs nothing and stops the
	// bridge acting on a backend it was never told about.
	if len(c.AllowedServers) > 0 && !c.ServerExplicitlyAllowed(host) {
		return ""
	}
	return c.BotSecret
}

// HasBotSecret reports whether any secret is configured at all.
func (c *Config) HasBotSecret() bool {
	return c.BotSecret != "" || len(c.BotSecrets) > 0
}

// ServerAllowed reports whether users may log into the given host.
// An empty allowlist permits any server.
func (c *Config) ServerAllowed(host string) bool {
	if len(c.AllowedServers) == 0 {
		return true
	}
	return c.ServerExplicitlyAllowed(host)
}

// ServerExplicitlyAllowed reports whether a host is named in the allowlist.
//
// This is the stricter question ServerAllowed cannot answer: an empty allowlist
// permits any public server, but naming a host is what marks an otherwise
// refused one — anything on a private or loopback address — as intentional.
func (c *Config) ServerExplicitlyAllowed(host string) bool {
	for _, allowed := range c.AllowedServers {
		if strings.EqualFold(strings.TrimSpace(allowed), host) {
			return true
		}
	}
	return false
}

func upgradeConfig(helper configupgrade.Helper) {
	helper.Copy(configupgrade.Str, "bot_secret")
	helper.Copy(configupgrade.Map, "bot_secrets")
	helper.Copy(configupgrade.Str, "bot_name")
	helper.Copy(configupgrade.Bool, "auto_enable_bot")
	helper.Copy(configupgrade.List, "allowed_servers")
	helper.Copy(configupgrade.Bool, "relay_unlinked_users")
	// Generated rather than left to the operator, so credentials are encrypted
	// without anyone having to opt in. Same shape as the bridge's own
	// pickle_key and shared_secret.
	if key, ok := helper.Get(configupgrade.Str, "credential_key"); !ok || key == "" || key == "generate" {
		helper.Set(configupgrade.Str, random.String(64), "credential_key")
	} else {
		helper.Copy(configupgrade.Str, "credential_key")
	}
	helper.Copy(configupgrade.Str, "login_timeout")
	helper.Copy(configupgrade.Str, "sync_interval")
}

// GetConfig implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	return ExampleConfig, &nc.Config, configupgrade.SimpleUpgrader(upgradeConfig)
}
