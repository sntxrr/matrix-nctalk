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
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// talkMessage is the bridge's normalised view of an incoming Talk message,
// assembled from either a webhook activity or a chat API response.
type talkMessage struct {
	Token     string
	MessageID int64

	ActorType string
	ActorID   string
	ActorName string

	// Text is the raw message with {placeholder} references intact.
	Text       string
	Parameters map[string]nctalk.MessageParam
	IsMarkdown bool

	// ParamsResolved records that Parameters came from a chat API call made as
	// this login, so a file's path in them is resolved against their own files
	// and can be fetched. The copy delivered over the bot webhook is not.
	ParamsResolved bool

	// SystemType is the raw system message type, or "message" for chat messages.
	SystemType string

	ReplyToID int64

	PublishedAt time.Time
	ReceivedAt  time.Time
}

// timestamp returns the best available time for the message.
func (m *talkMessage) timestamp() time.Time {
	if !m.PublishedAt.IsZero() {
		return m.PublishedAt
	}
	if !m.ReceivedAt.IsZero() {
		return m.ReceivedAt
	}
	return time.Now()
}

// isSystem reports whether this is a system message rather than a chat message.
func (m *talkMessage) isSystem() bool {
	return m.SystemType != "" && m.SystemType != "message"
}

// convertMessage turns a Talk message into Matrix content.
func (c *NCTalkClient) convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *talkMessage) (*bridgev2.ConvertedMessage, error) {
	// A shared file is a chat message whose entire body is a rich object, so it
	// becomes a Matrix media event rather than the text rendering of its name.
	if part := c.convertFilePart(ctx, portal, intent, msg); part != nil {
		converted := &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{part}}
		c.attachReply(converted, msg)
		return converted, nil
	}

	plain := renderTalkMessage(msg.Text, msg.Parameters)
	if strings.TrimSpace(plain) == "" {
		return nil, fmt.Errorf("message %d has no renderable content", msg.MessageID)
	}

	var content event.MessageEventContent
	if msg.IsMarkdown {
		// Talk's markdown is close enough to CommonMark for the shared renderer.
		content = format.RenderMarkdown(plain, true, false)
	} else {
		content = event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    plain,
		}
	}
	if msg.isSystem() {
		content.MsgType = event.MsgNotice
	}

	converted := &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: &content,
		}},
	}
	c.attachReply(converted, msg)
	return converted, nil
}

// attachReply points a converted message at the Talk message it replies to.
func (c *NCTalkClient) attachReply(converted *bridgev2.ConvertedMessage, msg *talkMessage) {
	if msg.ReplyToID > 0 {
		converted.ReplyTo = &networkid.MessageOptionalPartID{
			MessageID: makeMessageID(c.host(), msg.Token, msg.ReplyToID),
		}
	}
}

// convertFilePart builds the media part for a Talk message that shares a file,
// or nil when the message shares none.
//
// A failure to move the file is deliberately not fatal: it returns nil so the
// caller falls back to the text rendering, which at least names the file. A
// message that arrived is worth showing imperfectly rather than dropping.
func (c *NCTalkClient) convertFilePart(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *talkMessage) *bridgev2.ConvertedMessagePart {
	if msg.isSystem() || intent == nil {
		return nil
	}
	key := fileParamKey(msg.Parameters)
	if key == "" {
		return nil
	}

	// Whatever text surrounds the file placeholder is the caption Talk stored
	// with the share.
	caption := strings.TrimSpace(renderTalkMessage(msg.Text, omitParam(msg.Parameters, key)))

	part, err := c.convertFileFromTalk(ctx, portal, intent, msg, msg.Parameters[key], caption)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).
			Int64("talk_message_id", msg.MessageID).
			Msg("Could not bridge the shared file, falling back to its name as text")
		return nil
	}
	return part
}

// omitParam returns the parameter map with one key rendered as nothing, which
// is how the file placeholder is removed to leave just the caption.
func omitParam(params map[string]nctalk.MessageParam, key string) map[string]nctalk.MessageParam {
	out := make(map[string]nctalk.MessageParam, len(params))
	for k, v := range params {
		if k == key {
			// A blank rich object renders as the empty string rather than being
			// left as a literal "{file}" by the unknown-placeholder path.
			out[k] = nctalk.MessageParam{Type: paramTypeOmitted}
			continue
		}
		out[k] = v
	}
	return out
}

// paramTypeOmitted marks a placeholder that should render as nothing. It is not
// a Talk type; Talk never sends an empty one.
const paramTypeOmitted = "\x00omitted"

// renderTalkMessage substitutes Talk's Rich Object String placeholders.
//
// Talk sends message text containing {placeholder} references with the actual
// values in a parameter map, so the raw text alone reads as
// "{actor} shared {file}". Unknown placeholders are left as-is rather than
// dropped, so nothing silently disappears from a message.
func renderTalkMessage(text string, params map[string]nctalk.MessageParam) string {
	if text == "" || len(params) == 0 {
		return text
	}

	var b strings.Builder
	b.Grow(len(text))

	for i := 0; i < len(text); {
		openIdx := strings.IndexByte(text[i:], '{')
		if openIdx < 0 {
			b.WriteString(text[i:])
			break
		}
		openIdx += i
		closeIdx := strings.IndexByte(text[openIdx:], '}')
		if closeIdx < 0 {
			b.WriteString(text[i:])
			break
		}
		closeIdx += openIdx

		b.WriteString(text[i:openIdx])
		key := text[openIdx+1 : closeIdx]
		if param, ok := params[key]; ok {
			b.WriteString(renderParam(param))
		} else {
			// Not a placeholder, or one Talk did not supply a value for.
			b.WriteString(text[openIdx : closeIdx+1])
		}
		i = closeIdx + 1
	}
	return b.String()
}

// renderParam renders a single rich object as plain text.
//
// Files become real Matrix media before this is reached, so the file case here
// is the fallback for a share that could not be moved. Polls, deck cards and
// the rest have no Matrix equivalent, and degrade to something readable rather
// than vanishing from the message.
func renderParam(p nctalk.MessageParam) string {
	switch p.Type {
	case paramTypeOmitted:
		return ""
	case nctalk.ParamTypeUser, nctalk.ParamTypeGuest, nctalk.ParamTypeFederatedUser:
		if p.Name != "" {
			return p.Name
		}
		return p.ID
	case nctalk.ParamTypeFile:
		if p.Name != "" {
			return p.Name
		}
		return "file"
	case nctalk.ParamTypeGeoLocation:
		if p.Name != "" {
			return p.Name
		}
		return "location"
	default:
		if p.Name != "" {
			return p.Name
		}
		if p.ID != "" {
			return p.ID
		}
		return ""
	}
}
