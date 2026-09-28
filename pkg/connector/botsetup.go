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
	"context"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

// ensureBotEnabled makes sure the bridge bot is active in a conversation, so
// Talk will deliver its messages over the webhook.
//
// Talk exposes bot management to conversation moderators, which lets the bridge
// enable itself without admin access to the server. Where the logged-in user is
// not a moderator this is not possible, and the conversation needs a moderator
// to enable the bot by hand.
//
// Failures are recorded rather than retried on every sync, and are never fatal:
// a portal without the bot still works for outgoing messages.
func (c *NCTalkClient) ensureBotEnabled(ctx context.Context, portal *bridgev2.Portal, conv *nctalk.Conversation) {
	if !c.Main.Config.AutoEnableBot {
		return
	}

	meta, ok := portal.Metadata.(*PortalMetadata)
	if !ok {
		return
	}
	if meta.BotEnabled || meta.BotEnableFailed {
		return
	}

	if c.enableBridgeBot(ctx, conv) {
		meta.BotEnabled = true
		meta.BotEnableFailed = false
	} else {
		meta.BotEnableFailed = true
	}
}

// autoEnableBot enables the bridge bot in a conversation that has no portal
// yet.
//
// A portal is only ever created by a webhook, and Talk only sends webhooks for
// conversations the bot is enabled in, so without this a conversation created
// after login would never be bridged: ensureBotEnabled only runs for portals
// that already exist. Enabling the bot makes Talk deliver a Join activity,
// which is what creates the portal.
//
// Only group and public conversations qualify. Enabling a bot is announced to
// every participant, and doing it unasked in someone's one-to-one conversation,
// or in the user's own notes, is more than auto_enable_bot promises. Those can
// still be bridged by enabling the bot by hand.
//
// Each conversation is tried once per bridge run, whatever the outcome, so the
// hourly sync does not re-ask Talk about every conversation forever.
func (c *NCTalkClient) autoEnableBot(ctx context.Context, conv *nctalk.Conversation) {
	if !c.Main.Config.AutoEnableBot {
		return
	}
	if conv.Type != nctalk.RoomTypeGroup && conv.Type != nctalk.RoomTypePublic {
		return
	}
	if !conv.IsModerator() {
		return
	}

	c.botEnableMu.Lock()
	if c.botEnableTried[conv.Token] {
		c.botEnableMu.Unlock()
		return
	}
	if c.botEnableTried == nil {
		c.botEnableTried = make(map[string]bool)
	}
	c.botEnableTried[conv.Token] = true
	c.botEnableMu.Unlock()

	c.enableBridgeBot(ctx, conv)
}

// enableBridgeBot turns the bridge bot on in one conversation, and reports
// whether it is now enabled. Every reason it could not be is logged here.
func (c *NCTalkClient) enableBridgeBot(ctx context.Context, conv *nctalk.Conversation) bool {
	log := zerolog.Ctx(ctx).With().Str("token", conv.Token).Logger()

	if !conv.Bridgeable() {
		log.Debug().Int("conversation_type", conv.Type).Msg("Conversation cannot host the bridge bot")
		return false
	}
	if !conv.IsModerator() {
		log.Info().
			Msg("Not a moderator of this conversation, so the bridge bot cannot be enabled automatically; " +
				"ask a moderator to enable it in the conversation settings")
		return false
	}

	// The per-conversation bot list is the only way to learn the bot's ID
	// without admin API access, and it also tells us whether it is already on.
	bot, err := c.Client.FindBotByName(ctx, conv.Token, c.Main.Config.BotName)
	if err != nil {
		log.Warn().Err(err).
			Str("bot_name", c.Main.Config.BotName).
			Msg("Could not find the bridge bot; check that bot_name matches the name used with `occ talk:bot:install`")
		return false
	}

	if bot.State == nctalk.BotStateNoSetup {
		log.Warn().Msg("The bridge bot was installed with --no-setup, so moderators cannot enable it; " +
			"reinstall it without that flag or use `occ talk:bot:setup`")
		return false
	}

	if err := c.Client.EnableBot(ctx, conv.Token, bot.ID); err != nil {
		log.Warn().Err(err).Int64("bot_id", bot.ID).Msg("Failed to enable the bridge bot in this conversation")
		return false
	}

	log.Info().Int64("bot_id", bot.ID).Msg("Enabled the bridge bot in conversation")
	return true
}
