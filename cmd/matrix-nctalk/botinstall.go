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

package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// The bot install command's own limits, from spreed's BotInstall command. Talk
// rejects anything outside them, so they are checked here rather than after a
// round trip to a server the bridge cannot reach.
const (
	minBotSecretLength = 40
	maxBotSecretLength = 128
	maxBotNameLength   = 64
)

// placeholderPublicAddress is the value the Matrix connector treats as unset.
// A webhook URL built from it would be accepted by Talk and never work.
const placeholderPublicAddress = "https://bridge.example.com"

const defaultBotName = "Matrix Bridge"

const botDescription = "Bridges this conversation to Matrix"

// botInstallConfig is the handful of fields the helper reads out of the bridge
// config. It is deliberately parsed on its own rather than through the bridge's
// config machinery, which validates far more than this needs and refuses to
// load a config that has not been filled in yet — which is exactly when this
// command is useful.
type botInstallConfig struct {
	AppService struct {
		PublicAddress string `yaml:"public_address"`
	} `yaml:"appservice"`
	Network struct {
		BotSecret string `yaml:"bot_secret"`
		BotName   string `yaml:"bot_name"`
	} `yaml:"network"`
}

// runBotInstall prints the `occ talk:bot:install` command for this bridge.
//
// The bridge cannot run it: `occ` is on the Nextcloud server and needs shell
// access there. What it can do is get every argument right, which the manual
// route makes surprisingly hard — the order is name-secret-url-description, the
// secret has a length Talk enforces, and two of the three features are not
// defaults.
func runBotInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bot-install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("c", "config.yaml", "path to the bridge config")
	urlFlag := fs.String("url", "", "webhook URL Nextcloud should post to, if the config has no usable public address")
	nameFlag := fs.String("name", "", "bot name, if it should differ from the config")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: matrix-nctalk bot-install [options]\n\n"+
			"Prints the occ command that registers this bridge as a Talk bot.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := readBotInstallConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "Could not read %s: %v\n", *configPath, err)
		return 1
	}

	webhookURL, err := resolveWebhookURL(cfg, *urlFlag)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	name := firstNonEmpty(*nameFlag, cfg.Network.BotName, defaultBotName)
	if len(name) > maxBotNameLength {
		fmt.Fprintf(stderr, "The bot name is %d characters; Talk allows at most %d.\n", len(name), maxBotNameLength)
		return 1
	}

	secret, generated, err := resolveBotSecret(cfg.Network.BotSecret)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	fmt.Fprint(stdout, botInstallInstructions(name, secret, webhookURL, *configPath, generated))
	return 0
}

// readBotInstallConfig loads the config, treating a missing file as empty: this
// command is most useful before there is a config to read.
func readBotInstallConfig(path string) (*botInstallConfig, error) {
	var cfg botInstallConfig
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &cfg, nil
	} else if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// resolveWebhookURL works out where Nextcloud should post, preferring an
// explicit flag over the configured public address.
func resolveWebhookURL(cfg *botInstallConfig, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	address := strings.TrimRight(cfg.AppService.PublicAddress, "/")
	if address == "" || address == placeholderPublicAddress {
		return "", fmt.Errorf(
			"the config has no real appservice.public_address, so the webhook URL is unknown;\n" +
				"set it to the address Nextcloud can reach the bridge at, or pass --url")
	}
	return address + "/_nctalk/webhook", nil
}

// resolveBotSecret returns the configured secret, or a fresh one when the
// config has none, reporting which it was.
func resolveBotSecret(configured string) (secret string, generated bool, err error) {
	if configured != "" {
		if len(configured) < minBotSecretLength || len(configured) > maxBotSecretLength {
			return "", false, fmt.Errorf(
				"the configured network.bot_secret is %d characters; Talk requires between %d and %d,\n"+
					"so it would be rejected. Clear it to have a usable one generated",
				len(configured), minBotSecretLength, maxBotSecretLength)
		}
		return configured, false, nil
	}

	// 32 bytes of hex is 64 characters, comfortably inside Talk's range and
	// free of anything a shell would want to quote.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("could not generate a secret: %w", err)
	}
	return hex.EncodeToString(buf), true, nil
}

// botInstallInstructions renders what the operator has to do.
func botInstallInstructions(name, secret, webhookURL, configPath string, generated bool) string {
	var b strings.Builder

	b.WriteString("Run this on the Nextcloud server, as the web server user:\n\n")
	fmt.Fprintf(&b, "  occ talk:bot:install %s \\\n", shellQuote(name))
	fmt.Fprintf(&b, "      %s \\\n", secret)
	fmt.Fprintf(&b, "      %s \\\n", shellQuote(webhookURL))
	fmt.Fprintf(&b, "      %s \\\n", shellQuote(botDescription))
	b.WriteString("      --feature webhook --feature response --feature reaction\n\n")

	if generated {
		fmt.Fprintf(&b, "Then put the same secret in %s:\n\n", configPath)
		b.WriteString("  network:\n")
		fmt.Fprintf(&b, "      bot_secret: %q\n", secret)
		fmt.Fprintf(&b, "      bot_name: %q\n\n", name)
	} else {
		fmt.Fprintf(&b, "The secret is the one already in %s, so nothing there needs changing.\n\n", configPath)
	}

	b.WriteString("Three things about that command are easy to get wrong:\n\n")
	b.WriteString("  - The arguments are name, secret, url, description, in that order. Talk\n")
	b.WriteString("    accepts them in any order that parses and fails later if they are wrong.\n")
	b.WriteString("  - --feature reaction is not a default. Without it Talk silently never\n")
	b.WriteString("    delivers reactions, with no error anywhere to explain why.\n")
	b.WriteString("  - Do not pass --no-setup. It installs the bot in a state where moderators\n")
	b.WriteString("    cannot enable it, so the bridge cannot add itself to conversations and\n")
	b.WriteString("    every one of them needs `occ talk:bot:setup <botId> <token>` by hand.\n")

	return b.String()
}

// shellQuote wraps a value in single quotes so it survives being pasted into a
// shell, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
