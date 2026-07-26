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

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
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
}

var _ bridgev2.NetworkConnector = (*NCTalkConnector)(nil)

// Init implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) Init(bridge *bridgev2.Bridge) {
	nc.Bridge = bridge
	nc.HTTP = &http.Client{Timeout: 30 * time.Second}
}

// Start implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) Start(ctx context.Context) error {
	if nc.Config.BotSecret == "" {
		return fmt.Errorf("network.bot_secret is not set; install the bot with `occ talk:bot:install` and copy the shared secret into the config")
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
		BeeperBridgeType:     "github.com/sntxrr/matrix-nextcloud",
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

	client := nctalk.NewClient(meta.ServerURL, meta.Username, meta.AppPassword)
	client.HTTP = nc.HTTP

	login.Client = &NCTalkClient{
		Main:      nc,
		UserLogin: login,
		Client:    client,
		Bot:       nctalk.NewBotClient(meta.ServerURL, nc.Config.BotSecret, nc.HTTP),
	}
	return nil
}

// botClientFor returns a bot client for the given Nextcloud base URL.
func (nc *NCTalkConnector) botClientFor(baseURL string) *nctalk.BotClient {
	return nctalk.NewBotClient(baseURL, nc.Config.BotSecret, nc.HTTP)
}

// loginTimeout returns the configured Login Flow v2 timeout, with a default.
func (nc *NCTalkConnector) loginTimeout() time.Duration {
	if nc.Config.LoginTimeout > 0 {
		return nc.Config.LoginTimeout
	}
	return 15 * time.Minute
}
