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
	"crypto/rand"
	"encoding/hex"
	"html"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

// mentionParamPrefix marks the parameters Talk creates for mentions typed in a
// chat message: mention-user1, mention-call1, mention-guest1 and so on.
//
// Only these become Matrix mentions. System messages use the same rich object
// types for their {actor} and {user} placeholders, and turning those into
// mentions would ping people every time somebody joined or started a call.
const mentionParamPrefix = "mention-"

// mentionTarget resolves the Matrix user a mention of a Talk actor should
// ping.
type mentionTarget interface {
	// LoginMXID returns the real Matrix user behind a bridge login, if the
	// login exists.
	LoginMXID(ctx context.Context, loginID networkid.UserLoginID) (id.UserID, bool)
	// GhostMXID returns the Matrix ID of the ghost for a Talk actor.
	GhostMXID(userID networkid.UserID) id.UserID
}

// bridgeMentionTarget resolves mention targets through the running bridge.
type bridgeMentionTarget struct {
	br *bridgev2.Bridge
}

func (b bridgeMentionTarget) LoginMXID(ctx context.Context, loginID networkid.UserLoginID) (id.UserID, bool) {
	login, err := b.br.GetExistingUserLoginByID(ctx, loginID)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Str("login_id", string(loginID)).Msg("Could not look up the login behind a mention")
		return "", false
	}
	if login == nil || login.UserMXID == "" {
		return "", false
	}
	return login.UserMXID, true
}

func (b bridgeMentionTarget) GhostMXID(userID networkid.UserID) id.UserID {
	return b.br.Matrix.GhostIntent(userID).GetMXID()
}

// mentionTargets returns the resolver for mentions, or nil when no bridge is
// attached, in which case mentions stay plain display names.
func (c *NCTalkClient) mentionTargets() mentionTarget {
	if c.mentions != nil {
		return c.mentions
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.Matrix == nil {
		return nil
	}
	return bridgeMentionTarget{br: c.UserLogin.Bridge}
}

// mentionMXID maps a Talk mention to the Matrix user it should ping.
//
// A Nextcloud user who is logged into this bridge is the Matrix user behind
// that login: pinging their ghost would notify nobody. Anyone else is their
// ghost. Mentions the bridge cannot map, such as groups and federated users,
// report false and stay plain text.
func (c *NCTalkClient) mentionMXID(ctx context.Context, p nctalk.MessageParam) (id.UserID, bool) {
	targets := c.mentionTargets()
	if targets == nil || p.ID == "" {
		return "", false
	}
	host := c.host()

	switch p.Type {
	case nctalk.ParamTypeUser:
		if p.ID == c.meta().Username && c.UserLogin.UserMXID != "" {
			return c.UserLogin.UserMXID, true
		}
		if mxid, ok := targets.LoginMXID(ctx, makeUserLoginID(host, p.ID)); ok {
			return mxid, true
		}
		return targets.GhostMXID(makeUserID(host, nctalk.ActorUsers, p.ID)), true
	case nctalk.ParamTypeGuest:
		// Guest mentions carry the actor as guest/<id>.
		guestID, found := strings.CutPrefix(p.ID, "guest/")
		if !found || guestID == "" {
			return "", false
		}
		return targets.GhostMXID(makeUserID(host, nctalk.ActorGuests, guestID)), true
	}
	return "", false
}

// talkSegment is one piece of a Talk message: literal text, or a placeholder
// already rendered, optionally as a Matrix mention.
type talkSegment struct {
	text    string
	mention id.UserID
}

// segmentTalkMessage splits a Talk message into literal text and rendered
// placeholders, resolving mentions to Matrix users along the way.
//
// It reports whether anything in the message is a mention: a user pill, or an
// @all mention of the whole conversation.
func (c *NCTalkClient) segmentTalkMessage(ctx context.Context, msg *talkMessage) (segments []talkSegment, mentions event.Mentions, hasMentions bool) {
	text, params := msg.Text, msg.Parameters
	for i := 0; i < len(text); {
		openIdx := strings.IndexByte(text[i:], '{')
		if openIdx < 0 {
			segments = append(segments, talkSegment{text: text[i:]})
			break
		}
		openIdx += i
		closeIdx := strings.IndexByte(text[openIdx:], '}')
		if closeIdx < 0 {
			segments = append(segments, talkSegment{text: text[i:]})
			break
		}
		closeIdx += openIdx

		segments = append(segments, talkSegment{text: text[i:openIdx]})
		key := text[openIdx+1 : closeIdx]
		param, ok := params[key]
		switch {
		case !ok:
			segments = append(segments, talkSegment{text: text[openIdx : closeIdx+1]})
		case msg.isSystem() || !strings.HasPrefix(key, mentionParamPrefix):
			segments = append(segments, talkSegment{text: renderParam(param)})
		case param.Type == nctalk.ParamTypeCall:
			// @all in Talk mentions everybody in the conversation.
			mentions.Room = true
			hasMentions = true
			segments = append(segments, talkSegment{text: renderParam(param)})
		default:
			seg := talkSegment{text: renderParam(param)}
			if mxid, ok := c.mentionMXID(ctx, param); ok {
				seg.mention = mxid
				mentions.Add(mxid)
				hasMentions = true
			}
			segments = append(segments, seg)
		}
		i = closeIdx + 1
	}
	return segments, mentions, hasMentions
}

// pillHTML renders a Matrix mention pill.
func pillHTML(mxid id.UserID, name string) string {
	return `<a href="` + html.EscapeString(mxid.URI().MatrixToURL()) + `">` + html.EscapeString(name) + `</a>`
}

// renderTalkContent builds the Matrix content for a Talk chat message whose
// text contains mentions, so they arrive as pills with m.mentions set and
// notify the people they name.
func renderTalkContent(segments []talkSegment, mentions event.Mentions, markdown bool) event.MessageEventContent {
	if markdown {
		return renderMarkdownWithPills(segments, mentions)
	}

	var body, formatted strings.Builder
	for _, seg := range segments {
		body.WriteString(seg.text)
		if seg.mention != "" {
			formatted.WriteString(pillHTML(seg.mention, seg.text))
			continue
		}
		formatted.WriteString(strings.ReplaceAll(html.EscapeString(seg.text), "\n", "<br>"))
	}
	content := event.MessageEventContent{
		MsgType:  event.MsgText,
		Body:     body.String(),
		Mentions: &mentions,
	}
	if len(mentions.UserIDs) > 0 {
		content.Format = event.FormatHTML
		content.FormattedBody = formatted.String()
	}
	return content
}

// renderMarkdownWithPills renders Talk markdown with mentions as pills.
//
// Pills are HTML, which the markdown renderer escapes, so each mention goes
// through it as an opaque alphanumeric token and is swapped for its pill in
// the rendered HTML afterwards.
func renderMarkdownWithPills(segments []talkSegment, mentions event.Mentions) event.MessageEventContent {
	var source strings.Builder
	for _, seg := range segments {
		source.WriteString(seg.text)
	}
	prefix := mentionTokenPrefix(source.String())

	source.Reset()
	var pills []string
	for _, seg := range segments {
		if seg.mention == "" {
			source.WriteString(seg.text)
			continue
		}
		source.WriteString(prefix + strings.Repeat("x", len(pills)) + "z")
		pills = append(pills, pillHTML(seg.mention, seg.text))
	}

	rendered := format.RenderMarkdown(source.String(), true, false)
	htmlBody := rendered.FormattedBody
	if htmlBody == "" {
		htmlBody = html.EscapeString(rendered.Body)
	}
	// Tokens are prefix, then i x's, then z. The prefix is hex, so no token
	// is a substring of another.
	for i := range pills {
		htmlBody = strings.ReplaceAll(htmlBody, prefix+strings.Repeat("x", i)+"z", pills[i])
	}

	content := format.HTMLToContent(htmlBody)
	// The HTML parser finds the pills itself, but @all has no pill to find.
	content.Mentions = &mentions
	return content
}

// mentionTokenPrefix picks a token prefix that does not occur in the text, so
// a message cannot contain a string that is mistaken for a mention.
func mentionTokenPrefix(text string) string {
	for {
		buf := make([]byte, 8)
		_, _ = rand.Read(buf)
		prefix := "nctalkmention" + hex.EncodeToString(buf)
		if !strings.Contains(text, prefix) {
			return prefix
		}
	}
}
