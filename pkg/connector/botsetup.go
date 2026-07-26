package connector

import (
	"context"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
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

	log := zerolog.Ctx(ctx).With().Str("token", conv.Token).Logger()

	if !conv.Bridgeable() {
		log.Debug().Int("conversation_type", conv.Type).Msg("Conversation cannot host the bridge bot")
		meta.BotEnableFailed = true
		return
	}
	if !conv.IsModerator() {
		log.Info().
			Msg("Not a moderator of this conversation, so the bridge bot cannot be enabled automatically; " +
				"ask a moderator to enable it in the conversation settings")
		meta.BotEnableFailed = true
		return
	}

	// The per-conversation bot list is the only way to learn the bot's ID
	// without admin API access, and it also tells us whether it is already on.
	bot, err := c.Client.FindBotByName(ctx, conv.Token, c.Main.Config.BotName)
	if err != nil {
		log.Warn().Err(err).
			Str("bot_name", c.Main.Config.BotName).
			Msg("Could not find the bridge bot; check that bot_name matches the name used with `occ talk:bot:install`")
		meta.BotEnableFailed = true
		return
	}

	if bot.State == nctalk.BotStateNoSetup {
		log.Warn().Msg("The bridge bot was installed with --no-setup, so moderators cannot enable it; " +
			"reinstall it without that flag or use `occ talk:bot:setup`")
		meta.BotEnableFailed = true
		return
	}

	if err := c.Client.EnableBot(ctx, conv.Token, bot.ID); err != nil {
		log.Warn().Err(err).Int64("bot_id", bot.ID).Msg("Failed to enable the bridge bot in this conversation")
		meta.BotEnableFailed = true
		return
	}

	log.Info().Int64("bot_id", bot.ID).Msg("Enabled the bridge bot in conversation")
	meta.BotEnabled = true
	meta.BotEnableFailed = false
}
