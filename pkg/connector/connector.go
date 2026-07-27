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

// Package connector implements a mautrix-go bridgev2 network connector for
// Nextcloud Talk.
//
// Ingress uses Talk's bot webhook API, so messages are pushed to the bridge
// rather than polled. Egress uses each user's own app password so messages
// appear in Talk as the real Nextcloud user rather than as a relay bot.
package connector

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

// NCTalkConnector is the top-level bridgev2 network connector.
type NCTalkConnector struct {
	Bridge *bridgev2.Bridge
	Config Config

	// HTTP is shared by all outgoing Nextcloud requests.
	HTTP *http.Client

	// router maps incoming webhook events to the login that owns the portal.
	router *loginRouter
	// queue carries verified webhook events from the HTTP handler to the
	// workers, keeping the handler within Talk's five second budget.
	queue chan *pendingEvent
	// nonces remembers recently seen webhook randoms, so a captured request
	// cannot be replayed.
	nonces *nonceCache
	// dnsResolver overrides name lookup when checking where a server address
	// points. It is nil in production, where the system resolver is used.
	dnsResolver hostResolver
}

var _ bridgev2.NetworkConnector = (*NCTalkConnector)(nil)

// Init implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) Init(bridge *bridgev2.Bridge) {
	nc.Bridge = bridge
	nc.HTTP = &http.Client{Timeout: 30 * time.Second}
}

// Start implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) Start(ctx context.Context) error {
	if !nc.Config.HasBotSecret() {
		return fmt.Errorf("neither network.bot_secret nor network.bot_secrets is set; run `matrix-nctalk bot-install` for the " +
			"`occ talk:bot:install` command to run on the Nextcloud server, then copy the shared secret into the config")
	}
	if nc.Config.BotName == "" {
		return fmt.Errorf("network.bot_name is not set; it must match the name the bot was installed with")
	}
	return nc.registerWebhook(ctx)
}

// GetName implements bridgev2.NetworkConnector.
//
// This is called before the config is loaded, so it must not read config values.
func (nc *NCTalkConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName: "Nextcloud Talk",
		NetworkURL:  "https://nextcloud.com/talk/",
		// Set once the Talk logo has been uploaded to the bridge's homeserver;
		// an invalid mxc:// URI is worse than none.
		NetworkIcon:          "",
		NetworkID:            "nctalk",
		BeeperBridgeType:     "github.com/sntxrr/matrix-nctalk",
		DefaultPort:          29337,
		DefaultCommandPrefix: "!nctalk",
	}
}

// GetCapabilities implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		// Talk marks a conversation read as a whole rather than per message,
		// and does not implicitly mark sent messages as read.
		ImplicitReadReceipts: true,
	}
}

// GetBridgeInfoVersion implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}

// LoadUserLogin implements bridgev2.NetworkConnector.
//
// This runs under the bridge's global cache lock, so it only constructs the
// client; all network calls happen later in Connect.
func (nc *NCTalkConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta, ok := login.Metadata.(*UserLoginMetadata)
	if !ok {
		return fmt.Errorf("unexpected metadata type %T on login %s", login.Metadata, login.ID)
	}
	if meta.ServerURL == "" || meta.Username == "" {
		return fmt.Errorf("login %s has incomplete credentials", login.ID)
	}

	// The credential is decrypted here and lives in the client from now on; the
	// metadata keeps only the encrypted form. A failure is carried rather than
	// returned, so the login still loads and can report why it is unusable.
	appPassword, credentialErr := nc.decryptCredential(meta.AppPassword)

	client := nctalk.NewClient(meta.ServerURL, meta.Username, appPassword)
	client.HTTP = nc.HTTP

	login.Client = &NCTalkClient{
		Main:          nc,
		UserLogin:     login,
		Client:        client,
		Bot:           nc.botClientFor(meta.ServerURL),
		credentialErr: credentialErr,
	}
	return nil
}

// botClientFor returns a bot client for the given Nextcloud base URL, signing
// with the secret configured for that host.
func (nc *NCTalkConnector) botClientFor(baseURL string) *nctalk.BotClient {
	host, err := hostOf(baseURL)
	if err != nil {
		host = baseURL
	}
	return nctalk.NewBotClient(baseURL, nc.Config.BotSecretFor(host), nc.HTTP)
}

// loginTimeout returns the configured Login Flow v2 timeout, with a default.
func (nc *NCTalkConnector) loginTimeout() time.Duration {
	if nc.Config.LoginTimeout > 0 {
		return nc.Config.LoginTimeout
	}
	return 15 * time.Minute
}
