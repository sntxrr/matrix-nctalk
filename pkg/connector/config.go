package connector

import (
	_ "embed"
	"strings"
	"time"

	"go.mau.fi/util/configupgrade"
)

//go:embed example-config.yaml
var ExampleConfig string

// Config is the `network:` section of the bridge config.
type Config struct {
	BotSecret string `yaml:"bot_secret"`
	BotName   string `yaml:"bot_name"`

	AutoEnableBot bool `yaml:"auto_enable_bot"`

	AllowedServers []string `yaml:"allowed_servers"`

	RelayUnlinkedUsers bool `yaml:"relay_unlinked_users"`

	LoginTimeout time.Duration `yaml:"login_timeout"`
}

// ServerAllowed reports whether users may log into the given host.
// An empty allowlist permits any server.
func (c *Config) ServerAllowed(host string) bool {
	if len(c.AllowedServers) == 0 {
		return true
	}
	for _, allowed := range c.AllowedServers {
		if strings.EqualFold(strings.TrimSpace(allowed), host) {
			return true
		}
	}
	return false
}

func upgradeConfig(helper configupgrade.Helper) {
	helper.Copy(configupgrade.Str, "bot_secret")
	helper.Copy(configupgrade.Str, "bot_name")
	helper.Copy(configupgrade.Bool, "auto_enable_bot")
	helper.Copy(configupgrade.List, "allowed_servers")
	helper.Copy(configupgrade.Bool, "relay_unlinked_users")
	helper.Copy(configupgrade.Str, "login_timeout")
}

// GetConfig implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	return ExampleConfig, &nc.Config, configupgrade.SimpleUpgrader(upgradeConfig)
}
