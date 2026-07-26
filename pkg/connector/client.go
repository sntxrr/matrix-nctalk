package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/jsontime"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// Talk's own limits on modifying messages, from the chat API documentation.
const (
	talkEditMaxAge   = 24 * time.Hour
	talkDeleteMaxAge = 6 * time.Hour
	// talkMaxMessageLength is the hard limit the chat endpoint enforces.
	talkMaxMessageLength = 32000
)

// NCTalkClient is the per-login network client. One exists per logged-in
// Nextcloud account.
type NCTalkClient struct {
	Main      *NCTalkConnector
	UserLogin *bridgev2.UserLogin

	// Client makes OCS calls as the logged-in Nextcloud user.
	Client *nctalk.Client
	// Bot makes calls as the registered bridge bot, used only for relaying
	// Matrix users who have no linked Nextcloud account.
	Bot *nctalk.BotClient

	// caps caches the server's Talk capabilities, refreshed on Connect.
	caps *nctalk.Capabilities

	// queuer overrides where remote events are sent. It is nil in production,
	// where events go to the bridge; tests substitute a recorder.
	queuer eventQueuer
}

var _ bridgev2.NetworkAPI = (*NCTalkClient)(nil)

// eventQueuer accepts remote events for the bridge to process.
type eventQueuer interface {
	QueueRemoteEvent(login *bridgev2.UserLogin, evt bridgev2.RemoteEvent) bridgev2.EventHandlingResult
}

// events returns the destination for remote events.
func (c *NCTalkClient) events() eventQueuer {
	if c.queuer != nil {
		return c.queuer
	}
	return c.UserLogin.Bridge
}

func (c *NCTalkClient) meta() *UserLoginMetadata {
	return c.UserLogin.Metadata.(*UserLoginMetadata)
}

// host returns the Nextcloud hostname, which prefixes all network IDs.
func (c *NCTalkClient) host() string {
	return c.Client.Host()
}

// Connect implements bridgev2.NetworkAPI.
//
// Talk has no persistent connection: ingress arrives over the bot webhook. So
// connecting just validates the stored app password and caches capabilities.
func (c *NCTalkClient) Connect(ctx context.Context) {
	log := zerolog.Ctx(ctx)

	me, caps, err := c.Client.VerifyCredentials(ctx)
	if err != nil {
		if nctalk.IsUnauthorized(err) {
			c.UserLogin.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateBadCredentials,
				Error:      "nctalk-invalid-app-password",
				Message:    "The Nextcloud app password is no longer valid. Log in again with `login`.",
			})
			return
		}
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateUnknownError,
			Error:      "nctalk-connect-failed",
			Message:    err.Error(),
		})
		return
	}

	c.caps = caps
	c.meta().Features = caps.Features
	if err := c.UserLogin.Save(ctx); err != nil {
		log.Err(err).Msg("Failed to save cached Talk capabilities")
	}

	if !caps.Has(nctalk.CapBotsV1) {
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateUnknownError,
			Error:      "nctalk-no-bot-support",
			Message:    "This Nextcloud server's Talk version does not support bots (needs Talk 17.1 / Nextcloud 27.1 or newer).",
		})
		return
	}

	// Pre-populate the webhook routing cache so the first message in each
	// conversation does not pay for a participation probe.
	if c.Main.router != nil {
		if err := c.Main.router.Warm(ctx, c.UserLogin); err != nil {
			log.Warn().Err(err).Msg("Failed to pre-load conversation list for webhook routing")
		}
	}

	log.Info().
		Str("nextcloud_user", me.ID).
		Str("host", c.host()).
		Msg("Connected to Nextcloud Talk")

	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
}

// Disconnect implements bridgev2.NetworkAPI. There is no persistent connection
// to tear down.
func (c *NCTalkClient) Disconnect() {}

// IsLoggedIn implements bridgev2.NetworkAPI.
func (c *NCTalkClient) IsLoggedIn() bool {
	return c.meta().AppPassword != ""
}

// LogoutRemote implements bridgev2.NetworkAPI. It revokes the app password so
// logging out of the bridge also removes the entry from Nextcloud's security
// settings rather than leaving a live credential behind.
func (c *NCTalkClient) LogoutRemote(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.Client.BaseURL+"/ocs/v2.php/core/apppassword", nil)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to build app password revocation request")
		return
	}
	req.SetBasicAuth(c.meta().Username, c.meta().AppPassword)
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("Accept", "application/json")

	resp, err := c.Main.HTTP.Do(req)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to revoke Nextcloud app password")
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// IsThisUser implements bridgev2.NetworkAPI.
func (c *NCTalkClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	host, actorType, actorID, err := parseUserID(userID)
	if err != nil {
		return false
	}
	return actorType == nctalk.ActorUsers &&
		host == c.host() &&
		actorID == c.meta().Username
}

// GetChatInfo implements bridgev2.NetworkAPI.
func (c *NCTalkClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	host, token, err := parsePortalID(portal.ID)
	if err != nil {
		return nil, err
	}
	if host != c.host() {
		return nil, fmt.Errorf("portal %s belongs to a different Nextcloud server", portal.ID)
	}

	conv, err := c.Client.GetConversation(ctx, token)
	if err != nil {
		return nil, err
	}
	participants, err := c.Client.ListParticipants(ctx, token)
	if err != nil {
		return nil, err
	}

	members := &bridgev2.ChatMemberList{
		IsFull:           true,
		TotalMemberCount: len(participants),
		MemberMap:        make(bridgev2.ChatMemberMap, len(participants)),
	}
	for _, p := range participants {
		if !isBridgeableActor(p.ActorType) {
			continue
		}
		sender := c.eventSender(p.ActorType, p.ActorID)
		member := bridgev2.ChatMember{
			EventSender: sender,
			Membership:  event.MembershipJoin,
		}
		if p.IsModerator() {
			member.PowerLevel = ptr.Ptr(50)
		}
		if p.DisplayName != "" {
			member.UserInfo = &bridgev2.UserInfo{Name: ptr.Ptr(p.DisplayName)}
		}
		members.MemberMap.Set(member)
	}

	roomType := database.RoomTypeDefault
	if conv.IsOneToOne() {
		roomType = database.RoomTypeDM
	}

	info := &bridgev2.ChatInfo{
		Name:        ptr.Ptr(conv.DisplayName),
		Members:     members,
		Type:        &roomType,
		CanBackfill: true,
		ExtraUpdates: func(ctx context.Context, p *bridgev2.Portal) bool {
			meta := p.Metadata.(*PortalMetadata)
			before := *meta

			if meta.ConversationType != conv.Type {
				meta.ConversationType = conv.Type
			}
			// Without the bot enabled, Talk never delivers this conversation's
			// messages, so this is what makes a portal actually two-way.
			c.ensureBotEnabled(ctx, p, conv)

			return *meta != before
		},
	}
	if conv.Description != "" {
		info.Topic = ptr.Ptr(conv.Description)
	}
	if conv.IsCustomAvatar {
		info.Avatar = c.conversationAvatar(conv)
	}
	return info, nil
}

// conversationAvatar builds an Avatar that downloads the conversation's picture.
func (c *NCTalkClient) conversationAvatar(conv *nctalk.Conversation) *bridgev2.Avatar {
	url := c.Client.ConversationAvatarURL(conv.Token)
	// AvatarVersion changes whenever the picture does, so it makes a stable
	// cache key that avoids redownloading on every info sync.
	return &bridgev2.Avatar{
		ID:  networkid.AvatarID("conv:" + conv.Token + ":" + conv.AvatarVersion),
		Get: func(ctx context.Context) ([]byte, error) { return c.download(ctx, url) },
	}
}

// GetUserInfo implements bridgev2.NetworkAPI.
func (c *NCTalkClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	host, actorType, actorID, err := parseUserID(ghost.ID)
	if err != nil {
		return nil, err
	}
	if host != c.host() {
		return nil, fmt.Errorf("ghost %s belongs to a different Nextcloud server", ghost.ID)
	}

	info := &bridgev2.UserInfo{
		Identifiers: []string{fmt.Sprintf("nextcloud:%s/%s", host, actorID)},
		IsBot:       ptr.Ptr(actorType == nctalk.ActorBots),
	}

	// Only real users have profiles and avatars; guests and bots carry their
	// display name on each message instead.
	if actorType != nctalk.ActorUsers {
		return info, nil
	}

	if details, err := c.Client.GetUserDetails(ctx, actorID); err == nil && details.DisplayName != "" {
		info.Name = ptr.Ptr(details.DisplayName)
	} else if err != nil {
		// Reading another user's record needs privileges the login may lack.
		// That is expected, so fall back to the name from message metadata.
		zerolog.Ctx(ctx).Debug().Err(err).
			Str("nextcloud_user", actorID).
			Msg("Could not read user details, leaving name to message metadata")
	}

	avatarURL := c.Client.AvatarURL(actorID, 512)
	info.Avatar = &bridgev2.Avatar{
		ID:  networkid.AvatarID("user:" + actorID),
		Get: func(ctx context.Context) ([]byte, error) { return c.download(ctx, avatarURL) },
	}
	return info, nil
}

// download fetches a Nextcloud URL using the login's credentials.
func (c *NCTalkClient) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.meta().Username, c.meta().AppPassword)
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("User-Agent", nctalk.UserAgent)

	resp, err := c.Main.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// eventSender builds the bridgev2 sender for a Talk actor, marking messages
// from this login's own account so they are attributed correctly.
func (c *NCTalkClient) eventSender(actorType, actorID string) bridgev2.EventSender {
	isSelf := actorType == nctalk.ActorUsers && actorID == c.meta().Username
	sender := bridgev2.EventSender{
		IsFromMe: isSelf,
		Sender:   makeUserID(c.host(), actorType, actorID),
	}
	if isSelf {
		sender.SenderLogin = c.UserLogin.ID
	}
	return sender
}

// GetCapabilities implements bridgev2.NetworkAPI.
func (c *NCTalkClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	caps := c.caps
	if caps == nil {
		// Fall back to the capability list cached at login time so a portal
		// opened before the first successful Connect still advertises sanely.
		caps = &nctalk.Capabilities{Features: c.meta().Features}
	}

	supported := event.CapLevelFullySupported
	unsupported := event.CapLevelUnsupported

	feats := &event.RoomFeatures{
		ID:            "github.com/sntxrr/matrix-nextcloud/1",
		MaxTextLength: talkMaxMessageLength,
		Formatting: event.FormattingFeatureMap{
			event.FmtBold:          supported,
			event.FmtItalic:        supported,
			event.FmtStrikethrough: supported,
			event.FmtInlineCode:    supported,
			event.FmtCodeBlock:     supported,
			event.FmtBlockquote:    supported,
			event.FmtInlineLink:    supported,
			event.FmtUnorderedList: supported,
			event.FmtOrderedList:   supported,
			event.FmtUserLink:      supported,
		},
		Reply:               supported,
		ReadReceipts:        caps.Has(nctalk.CapChatReadStatus),
		TypingNotifications: false,
		Reaction:            unsupported,
		Edit:                unsupported,
		Delete:              unsupported,
	}

	if caps.Has(nctalk.CapReactions) {
		feats.Reaction = supported
		feats.CustomEmojiReactions = false
	}
	if caps.Has(nctalk.CapEditMessages) {
		feats.Edit = supported
		feats.EditMaxAge = ptr.Ptr(jsontime.S(talkEditMaxAge))
	}
	if caps.Has(nctalk.CapDeleteMessages) {
		feats.Delete = supported
		feats.DeleteMaxAge = ptr.Ptr(jsontime.S(talkDeleteMaxAge))
	}
	if caps.Has(nctalk.CapThreads) {
		feats.Thread = supported
	}
	return feats
}

// HandleMatrixMessage implements bridgev2.NetworkAPI.
func (c *NCTalkClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return nil, fmt.Errorf("sending to Nextcloud Talk is not implemented yet")
}
