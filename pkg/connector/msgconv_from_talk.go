package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	if msg.ReplyToID > 0 {
		converted.ReplyTo = &networkid.MessageOptionalPartID{
			MessageID: makeMessageID(c.host(), msg.Token, msg.ReplyToID),
		}
	}
	return converted, nil
}

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
// Media and interactive objects are handled properly in later milestones; for
// now every object type degrades to something readable rather than vanishing.
func renderParam(p nctalk.MessageParam) string {
	switch p.Type {
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
