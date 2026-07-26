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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig puts a config in a temporary directory and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("could not write the test config: %v", err)
	}
	return path
}

// runBotInstallForTest runs the command and returns its output and exit code.
func runBotInstallForTest(args ...string) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer
	code = runBotInstall(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestBotInstallUsesTheConfiguredSecret(t *testing.T) {
	secret := strings.Repeat("a", 48)
	path := writeConfig(t, `
appservice:
    public_address: https://bridge.example.org
network:
    bot_secret: "`+secret+`"
    bot_name: My Bridge
`)

	stdout, stderr, code := runBotInstallForTest("-c", path)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, secret) {
		t.Error("the configured secret should be the one in the printed command")
	}
	if !strings.Contains(stdout, "'My Bridge'") {
		t.Error("the bot name should come from the config")
	}
	// The webhook path is not something the operator should have to know.
	if !strings.Contains(stdout, "'https://bridge.example.org/_nctalk/webhook'") {
		t.Errorf("webhook URL missing or wrong:\n%s", stdout)
	}
	if strings.Contains(stdout, "put the same secret") {
		t.Error("nothing needs changing in a config that already has a usable secret")
	}
}

func TestBotInstallGeneratesAConformingSecret(t *testing.T) {
	path := writeConfig(t, "appservice:\n    public_address: https://bridge.example.org\n")

	stdout, stderr, code := runBotInstallForTest("-c", path)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "put the same secret") {
		t.Error("a generated secret is useless unless the operator is told to save it")
	}

	secret := secretFromOutput(t, stdout)
	// Talk enforces this range and rejects the install otherwise, which is the
	// whole reason not to have people invent their own.
	if len(secret) < minBotSecretLength || len(secret) > maxBotSecretLength {
		t.Errorf("generated a %d character secret, outside Talk's %d-%d",
			len(secret), minBotSecretLength, maxBotSecretLength)
	}
	if secret != strings.TrimSpace(secret) || strings.ContainsAny(secret, " '\"\\$`") {
		t.Errorf("secret %q contains something a shell would mangle", secret)
	}

	// Two runs must not produce the same secret.
	second, _, _ := runBotInstallForTest("-c", path)
	if secretFromOutput(t, second) == secret {
		t.Error("the generated secret is not random")
	}
}

// secretFromOutput pulls the secret out of the printed occ command, which is
// the only line that is a bare unquoted argument.
func secretFromOutput(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), `\`))
		if line == "" || strings.ContainsAny(line, " '") {
			continue
		}
		return line
	}
	t.Fatalf("no secret in the output:\n%s", out)
	return ""
}

func TestBotInstallRejectsASecretTalkWouldRefuse(t *testing.T) {
	path := writeConfig(t, `
appservice:
    public_address: https://bridge.example.org
network:
    bot_secret: "too-short"
`)

	_, stderr, code := runBotInstallForTest("-c", path)
	if code == 0 {
		t.Fatal("expected a failure for a secret shorter than Talk's minimum")
	}
	if !strings.Contains(stderr, "40") {
		t.Errorf("the error should name the limit, got: %s", stderr)
	}
}

func TestBotInstallNeedsARealPublicAddress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
	}{
		{name: "unset", config: "network:\n    bot_name: Bridge\n"},
		// The Matrix connector treats this exact value as unset and then exposes
		// no HTTP router, so a webhook URL built from it would never work.
		{name: "left as the example", config: "appservice:\n    public_address: https://bridge.example.com\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.config)

			_, stderr, code := runBotInstallForTest("-c", path)
			if code == 0 {
				t.Fatal("expected a failure when the webhook URL cannot be known")
			}
			if !strings.Contains(stderr, "--url") {
				t.Errorf("the error should offer the way out, got: %s", stderr)
			}
		})
	}
}

func TestBotInstallWorksBeforeThereIsAConfig(t *testing.T) {
	// This command is most useful before the bridge has ever run, so a missing
	// config is an ordinary case rather than an error.
	missing := filepath.Join(t.TempDir(), "not-created-yet.yaml")

	stdout, stderr, code := runBotInstallForTest("-c", missing, "--url", "https://bridge.example.org/_nctalk/webhook")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "'"+defaultBotName+"'") {
		t.Errorf("expected the default bot name, got:\n%s", stdout)
	}
}

func TestBotInstallOverridesTheConfiguredName(t *testing.T) {
	path := writeConfig(t, `
appservice:
    public_address: https://bridge.example.org
network:
    bot_name: From The Config
`)

	stdout, _, code := runBotInstallForTest("-c", path, "--name", "From The Flag")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "'From The Flag'") || strings.Contains(stdout, "From The Config") {
		t.Errorf("the flag should win over the config:\n%s", stdout)
	}
}

func TestBotInstallQuotesForTheShell(t *testing.T) {
	path := writeConfig(t, "appservice:\n    public_address: https://bridge.example.org\n")

	stdout, _, code := runBotInstallForTest("-c", path, "--name", "Don's Bridge")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	// Pasting an unescaped apostrophe into a shell leaves it hanging on an open
	// quote, which is a confusing way to find out.
	if !strings.Contains(stdout, `'Don'\''s Bridge'`) {
		t.Errorf("the name was not safely quoted:\n%s", stdout)
	}
}

func TestBotInstallRejectsAnOverlongName(t *testing.T) {
	path := writeConfig(t, "appservice:\n    public_address: https://bridge.example.org\n")

	_, stderr, code := runBotInstallForTest("-c", path, "--name", strings.Repeat("n", maxBotNameLength+1))
	if code == 0 {
		t.Fatal("expected a failure for a name longer than Talk allows")
	}
	if !strings.Contains(stderr, "64") {
		t.Errorf("the error should name the limit, got: %s", stderr)
	}
}

func TestBotInstallNamesTheNonObviousFlags(t *testing.T) {
	path := writeConfig(t, "appservice:\n    public_address: https://bridge.example.org\n")

	stdout, _, code := runBotInstallForTest("-c", path)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	// Omitting the reaction feature means Talk silently never delivers Like or
	// Undo, and --no-setup blocks the bridge from enabling itself. Both cost a
	// long debugging session, so the command must get them right and say why.
	for _, want := range []string{
		"--feature webhook", "--feature response", "--feature reaction",
		"--no-setup", "name, secret, url, description",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not mention %q", want)
		}
	}
}

func TestBotInstallReportsAnUnreadableConfig(t *testing.T) {
	path := writeConfig(t, "appservice: [this is not a mapping\n")

	_, stderr, code := runBotInstallForTest("-c", path)
	if code == 0 {
		t.Fatal("expected a failure for a config that does not parse")
	}
	if !strings.Contains(stderr, "Could not read") {
		t.Errorf("unhelpful error: %s", stderr)
	}
}
