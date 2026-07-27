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
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/jsontime"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

// Talk's own limits on modifying messages, from the chat API documentation.
const (
	talkEditMaxAge   = 24 * time.Hour
	talkDeleteMaxAge = 6 * time.Hour
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
	// mxidParser overrides how mention pills are resolved to Talk actors. It is
	// nil in production, where the bridge's Matrix connector is used.
	mxidParser ghostParser
	// downloader overrides where outgoing media is fetched from. It is nil in
	// production, where the bridge bot is used.
	downloader mediaDownloader
	// portalFinder overrides where existing portals are looked up. It is nil in
	// production, where the bridge is used.
	portalFinder portalLookup

	// syncMu guards syncCancel, which stops the periodic conversation resync.
	syncMu     sync.Mutex
	syncCancel context.CancelFunc

	// credentialErr records that the stored app password could not be read,
	// which Connect turns into a bad-credentials state rather than a stream of
	// confusing authentication failures against Nextcloud.
	credentialErr error
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

	if c.credentialErr != nil {
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "nctalk-undecryptable-credential",
			Message: "The stored Nextcloud app password could not be decrypted, which usually means " +
				"network.credential_key changed. Log in again with `login`.",
		})
		log.Err(c.credentialErr).Msg("Cannot use this login until it is re-authenticated")
		return
	}

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
	c.reencryptCredential(ctx)
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

	// Nothing Talk sends can cover a period when the bridge was not listening,
	// so the resync loop is what closes that gap, starting with a pass now.
	c.startPeriodicSync()

	log.Info().
		Str("nextcloud_user", me.ID).
		Str("host", c.host()).
		Msg("Connected to Nextcloud Talk")

	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
}

// Disconnect implements bridgev2.NetworkAPI. There is no persistent connection
// to tear down, only the background resync loop.
func (c *NCTalkClient) Disconnect() {
	c.stopPeriodicSync()
}

// IsLoggedIn implements bridgev2.NetworkAPI.
//
// The usable credential is the decrypted one held by the client, not the stored
// form, so a login whose credential will not decrypt correctly reports itself
// as logged out.
func (c *NCTalkClient) IsLoggedIn() bool {
	return c.credentialErr == nil && c.Client.AppPassword != ""
}

// reencryptCredential rewrites a credential that predates the bridge storing
// them encrypted. Connect saves the login anyway, so this rides along with it.
func (c *NCTalkClient) reencryptCredential(ctx context.Context) {
	meta := c.meta()
	if meta.AppPassword == "" || isEncryptedCredential(meta.AppPassword) {
		return
	}
	stored, err := c.Main.encryptCredential(c.Client.AppPassword)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).
			Msg("Could not encrypt the stored app password; it stays in the clear until this is fixed")
		return
	}
	meta.AppPassword = stored
	zerolog.Ctx(ctx).Info().Msg("Encrypted a Nextcloud app password that was stored in the clear")
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
	req.SetBasicAuth(c.Client.Username, c.Client.AppPassword)
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
	req.SetBasicAuth(c.Client.Username, c.Client.AppPassword)
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
	caps := c.capabilities()

	supported := event.CapLevelFullySupported
	unsupported := event.CapLevelUnsupported

	feats := &event.RoomFeatures{
		ID:            "github.com/sntxrr/matrix-nctalk/1",
		MaxTextLength: nctalk.MaxChatLength,
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
		Reply:        supported,
		ReadReceipts: caps.Has(nctalk.CapChatReadStatus),
		// Talk carries typing state over its signaling channel rather than OCS,
		// and bots have no way into it at all, so there is nothing to bridge.
		TypingNotifications: false,
		Reaction:            unsupported,
		Edit:                unsupported,
		Delete:              unsupported,
	}

	if caps.Has(nctalk.CapReactions) {
		feats.Reaction = supported
		// Talk validates that a reaction is a single emoji, so Matrix's custom
		// image reactions have nowhere to go.
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
	if caps.AttachmentsAllowed() {
		feats.File = talkFileFeatures()
	}
	return feats
}

// talkFileFeatures describes what the bridge will carry into Talk.
//
// Nextcloud stores whatever it is given, so no MIME type is refused; the only
// real ceiling is what the bridge is willing to hold in memory. Every kind of
// file takes the same route — upload to the user's files, share into the
// conversation — so they all get the same entry.
func talkFileFeatures() event.FileFeatureMap {
	file := &event.FileFeatures{
		MimeTypes: map[string]event.CapabilitySupportLevel{
			"*/*": event.CapLevelFullySupported,
		},
		// Talk carries a caption on the share itself, so it survives as one
		// message rather than being split into a file plus a comment.
		Caption:          event.CapLevelFullySupported,
		MaxCaptionLength: nctalk.MaxChatLength,
		MaxSize:          maxMediaSize,
	}
	return event.FileFeatureMap{
		event.MsgImage: file,
		event.MsgVideo: file,
		event.MsgAudio: file,
		event.MsgFile:  file,
		// Talk has its own voice message type the bridge does not produce, so a
		// Matrix voice note lands as an ordinary audio attachment. That is worth
		// accepting rather than rejecting the message.
		event.CapMsgVoice: file,
		event.CapMsgGIF:   file,
	}
}

// HandleMatrixMessage implements bridgev2.NetworkAPI.
//
// Messages are posted with the sender's own app password so they appear in Talk
// as the real Nextcloud user. The Talk message ID is returned so that when the
// bot webhook delivers the bridge's own message back, bridgev2 recognises it as
// already bridged instead of echoing it into the room a second time.
func (c *NCTalkClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	host, token, err := parsePortalID(msg.Portal.ID)
	if err != nil {
		return nil, err
	}
	if host != c.host() {
		return nil, fmt.Errorf("portal %s belongs to a different Nextcloud server", msg.Portal.ID)
	}

	// A file goes through WebDAV and a conversation share rather than the chat
	// endpoint, so it diverges before any text conversion.
	if msg.Content != nil && isMediaMsgType(msg.Content.MsgType) {
		return c.handleMatrixMedia(ctx, host, token, msg)
	}

	out, err := c.convertToTalk(ctx, token, msg)
	if err != nil {
		return nil, err
	}

	// OrigSender is only set when the Matrix sender has no Nextcloud account of
	// their own and the portal has a relay login, in which case bridgev2 has
	// already prefixed the text with their display name.
	if msg.OrigSender != nil {
		return c.relayMatrixMessage(ctx, host, token, out)
	}

	sent, err := c.sendToTalk(ctx, token, out)
	if err != nil {
		return nil, err
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:       makeMessageID(host, token, sent.ID),
			SenderID: makeUserID(host, nctalk.ActorUsers, c.meta().Username),
			// Talk's own timestamp, not the Matrix one, because Talk measures its
			// edit and delete windows from it.
			Timestamp: time.Unix(sent.Timestamp, 0),
			Metadata:  &MessageMetadata{ReferenceID: out.ReferenceID},
		},
		// Talk message IDs are a per-server monotonic sequence, which is the same
		// stream order incoming messages carry.
		StreamOrder: sent.ID,
	}, nil
}

// sendToTalk posts a converted message as the logged-in Nextcloud user.
func (c *NCTalkClient) sendToTalk(ctx context.Context, token string, out *talkOutgoing) (*nctalk.Message, error) {
	req := nctalk.SendMessageRequest{
		Message:     out.Text,
		ReplyTo:     out.ReplyTo,
		ThreadID:    out.ThreadID,
		ReferenceID: out.ReferenceID,
	}

	sent, err := c.Client.SendMessage(ctx, token, req)
	if err == nil {
		return sent, nil
	}

	// Talk answers 400 when it will not accept the parent of a reply — it was
	// deleted, it is a system message, or the thread does not match. The text is
	// still worth delivering, so retry once without the relation rather than
	// failing a message the user has already sent.
	if nctalk.IsBadRequest(err) && (req.ReplyTo > 0 || req.ThreadID > 0) {
		zerolog.Ctx(ctx).Warn().Err(err).
			Int64("reply_to", req.ReplyTo).
			Int64("thread_id", req.ThreadID).
			Msg("Talk rejected the message relation, resending without it")
		req.ReplyTo, req.ThreadID = 0, 0
		if retried, retryErr := c.Client.SendMessage(ctx, token, req); retryErr == nil {
			return retried, nil
		}
	}
	return nil, c.wrapSendError(ctx, err)
}

// relayMatrixMessage posts a message on behalf of a Matrix user who has no
// Nextcloud account, using the registered bridge bot.
//
// The bot endpoint acknowledges without reporting the ID Talk assigned, so the
// message is recorded under an ID derived from its reference instead. That is
// enough to keep the Matrix event mapped, but Talk-side relations to a relayed
// message cannot be resolved. Talk does not deliver bot-authored messages to
// bots, so no echo arrives to fill the gap in either.
func (c *NCTalkClient) relayMatrixMessage(ctx context.Context, host, token string, out *talkOutgoing) (*bridgev2.MatrixMessageResponse, error) {
	if !c.Main.Config.RelayUnlinkedUsers {
		return nil, errRelayDisabled
	}
	if err := c.Bot.SendMessage(ctx, token, out.Text, out.ReferenceID, out.ReplyTo, false); err != nil {
		return nil, c.wrapSendError(ctx, err)
	}

	zerolog.Ctx(ctx).Debug().
		Str("token", token).
		Str("reference_id", out.ReferenceID).
		Msg("Relayed a Matrix message to Talk as the bridge bot")

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID: makeRelayedMessageID(host, token, out.ReferenceID),
			// The relay login owns the portal, so attribute the row to it rather
			// than inventing an actor ID for the bot, which Talk never tells us.
			SenderID: makeUserID(host, nctalk.ActorUsers, c.meta().Username),
			Metadata: &MessageMetadata{ReferenceID: out.ReferenceID, SentViaBot: true},
		},
	}, nil
}

// wrapSendError turns a Talk rejection into the message status the Matrix client
// should show, and flags the login when its credentials have stopped working.
func (c *NCTalkClient) wrapSendError(ctx context.Context, err error) error {
	switch {
	case nctalk.IsTooLarge(err):
		return errMessageTooLong
	case nctalk.IsForbidden(err):
		return errNotAllowedInConversation
	case nctalk.IsUnauthorized(err):
		// Nothing else will notice until the next Connect, which may be days
		// away, so surface it now: every later send would fail the same way.
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "nctalk-invalid-app-password",
			Message:    "The Nextcloud app password is no longer valid. Log in again with `login`.",
		})
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Talk rejected the login's app password while sending")
		return err
	default:
		return err
	}
}
