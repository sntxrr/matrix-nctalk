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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

// Message status errors specific to sending into Talk. These follow the shape of
// bridgev2's own status errors so Element shows an actionable reason next to the
// message rather than a generic failure.
var (
	errMessageTooLong = bridgev2.WrapErrorInStatus(errors.New("message is longer than Nextcloud Talk allows")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusUnsupported)
	errEmptyMessage = bridgev2.WrapErrorInStatus(errors.New("message has no text content")).
			WithIsCertain(true).WithErrorAsMessage().WithSendNotice(false).
			WithErrorReason(event.MessageStatusUnsupported)
	errNotAllowedInConversation = bridgev2.WrapErrorInStatus(errors.New("you are not allowed to post in this Talk conversation")).
					WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
					WithErrorReason(event.MessageStatusUnsupported)
	errRelayDisabled = bridgev2.WrapErrorInStatus(errors.New("you have no linked Nextcloud account, and relaying is disabled on this bridge")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusUnsupported)
)

// talkOutgoing is a Talk chat message assembled from a Matrix event.
type talkOutgoing struct {
	Text        string
	ReplyTo     int64
	ThreadID    int64
	ReferenceID string
}

// ghostParser resolves a Matrix user ID to the network user ID it represents.
// Only mention pills need this, and only bridgev2's Matrix connector implements
// it in production; tests substitute a stub.
type ghostParser interface {
	ParseGhostMXID(id.UserID) (networkid.UserID, bool)
}

// ghosts returns the resolver used to turn mention pills into Talk mentions, or
// nil when no bridge is attached, in which case pills degrade to display names.
func (c *NCTalkClient) ghosts() ghostParser {
	if c.mxidParser != nil {
		return c.mxidParser
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil {
		return nil
	}
	return c.UserLogin.Bridge.Matrix
}

// convertToTalk turns an outgoing Matrix message into the Talk chat API call
// that reproduces it.
func (c *NCTalkClient) convertToTalk(ctx context.Context, token string, msg *bridgev2.MatrixMessage) (*talkOutgoing, error) {
	if msg.Content == nil {
		return nil, bridgev2.ErrUnexpectedParsedContentType
	}

	text, err := c.renderOutgoingText(ctx, msg.Content)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, errEmptyMessage
	}
	// Talk counts characters, not bytes, and rejects overlong messages with 413.
	// Catching it here turns a failed request into an immediate clear status.
	if utf8.RuneCountInString(text) > nctalk.MaxChatLength {
		return nil, errMessageTooLong
	}

	out := &talkOutgoing{
		Text: text,
		// The Matrix event ID is the natural idempotency key, and Talk stores the
		// reference verbatim, so a resend reuses it rather than looking like a
		// second message.
		ReferenceID: referenceID(msg.Event.ID),
	}
	out.ReplyTo = c.talkTargetID(ctx, token, msg.ReplyTo, "reply")
	// Talk derives the thread from the parent message, and rejects being given
	// both, so the thread ID is only useful for a threaded message that does not
	// quote anything.
	if out.ReplyTo == 0 {
		out.ThreadID = c.talkTargetID(ctx, token, msg.ThreadRoot, "thread root")
	}
	return out, nil
}

// talkTargetID resolves a bridged message the outgoing event points at into its
// Talk message ID.
//
// A target the bridge cannot resolve loses only the relation, so this logs and
// returns zero rather than failing the send: delivering the text unthreaded is
// better than dropping the message.
func (c *NCTalkClient) talkTargetID(ctx context.Context, token string, target *database.Message, kind string) int64 {
	if target == nil {
		return 0
	}
	host, targetToken, messageID, err := parseMessageID(target.ID)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).
			Str("target_id", string(target.ID)).
			Msgf("Dropping %s relation with an unparseable message ID", kind)
		return 0
	}
	// Talk needs the parent to be in the same conversation. Cross-conversation
	// replies exist in the API but need the parent's token, which is out of scope
	// until portals can reference each other.
	if host != c.host() || targetToken != token {
		zerolog.Ctx(ctx).Debug().
			Str("target_id", string(target.ID)).
			Str("token", token).
			Msgf("Dropping %s relation to a message in another conversation", kind)
		return 0
	}
	return messageID
}

// renderOutgoingText produces the Talk message text for a Matrix message.
//
// bridgev2 strips the rich reply fallback before handing the event over, so the
// body here is already just what the user wrote.
func (c *NCTalkClient) renderOutgoingText(ctx context.Context, content *event.MessageEventContent) (string, error) {
	switch content.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
	default:
		// Media, location and everything else arrives with file support in M4.
		return "", bridgev2.ErrUnsupportedMessageType
	}

	text := content.Body
	if content.Format == event.FormatHTML && content.FormattedBody != "" {
		// Talk renders markdown, and Matrix's HTML subset maps onto it closely
		// enough that the shared parser is the right tool.
		text = c.htmlParser().Parse(content.FormattedBody, format.NewContext(ctx))
	}
	text = strings.TrimSpace(text)

	if content.MsgType == event.MsgEmote && text != "" {
		// Talk has no emote message type. Italics is the closest rendering, and
		// reads the same way Matrix clients show "* alice waves".
		text = "*" + text + "*"
	}
	return text, nil
}

// htmlParser returns the Matrix HTML to Talk markdown parser, which is the
// shared markdown parser with mention pills rewritten into Talk mentions.
func (c *NCTalkClient) htmlParser() *format.HTMLParser {
	parser := *format.MarkdownHTMLParser
	parser.PillConverter = c.talkPillConverter
	return &parser
}

// talkPillConverter renders a Matrix pill as a Talk mention where the target is
// an actor on this Nextcloud server, and otherwise leaves the default rendering
// alone so nothing turns into a broken mention.
func (c *NCTalkClient) talkPillConverter(displayname, mxid, eventID string, ctx format.Context) string {
	if eventID == "" && strings.HasPrefix(mxid, "@") {
		if mention, ok := c.talkMention(id.UserID(mxid)); ok {
			return mention
		}
	}
	return format.DefaultPillConverter(displayname, mxid, eventID, ctx)
}

// talkMention renders a Matrix user as Talk's textual mention syntax, which Talk
// parses out of the message body and turns into a real mention with a
// notification.
//
// It reports false for anyone it cannot map, including ghosts belonging to a
// different Nextcloud server bridged by the same bridge.
func (c *NCTalkClient) talkMention(mxid id.UserID) (string, bool) {
	parser := c.ghosts()
	if parser == nil {
		return "", false
	}
	ghostID, ok := parser.ParseGhostMXID(mxid)
	if !ok {
		return "", false
	}
	host, actorType, actorID, err := parseUserID(ghostID)
	if err != nil || host != c.host() {
		return "", false
	}

	// The mention is delimited by double quotes, so an actor ID containing one
	// would produce a mention that ends early and leaks the rest as text.
	if strings.Contains(actorID, `"`) {
		return "", false
	}

	switch actorType {
	case nctalk.ActorUsers:
		return `@"` + actorID + `"`, true
	case nctalk.ActorGuests:
		return `@"guest/` + actorID + `"`, true
	case nctalk.ActorEmails:
		return `@"email/` + actorID + `"`, true
	case nctalk.ActorFederatedUsers:
		return `@"federated_user/` + actorID + `"`, true
	default:
		// Bots and deleted users have no mention form.
		return "", false
	}
}

// referenceID derives Talk's referenceId from the Matrix event ID.
//
// Talk truncates the reference to 64 characters, so hashing keeps it intact for
// any event ID length while staying deterministic: a retried send reuses the
// same reference instead of looking like a new message.
func referenceID(eventID id.EventID) string {
	sum := sha256.Sum256([]byte(eventID))
	return hex.EncodeToString(sum[:])
}
