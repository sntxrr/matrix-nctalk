package nctalk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// MaxChatLength is Talk's hard limit on the length of a chat message, from
// ChatManager::MAX_CHAT_LENGTH in spreed. Longer messages are rejected with 413.
const MaxChatLength = 32000

// maxReferenceIDLength is how much of a referenceId Talk stores; it truncates
// anything longer rather than rejecting it, which would silently break lookups.
const maxReferenceIDLength = 64

// ErrMessageNotReturned means Talk accepted the message but did not include it
// in the response. Talk omits the message when the sending participant cannot
// see it, so the message exists but its ID is unknown to us.
var ErrMessageNotReturned = errors.New("nctalk: Talk accepted the message but did not return it")

// SendMessageRequest is the payload of a chat message send.
type SendMessageRequest struct {
	// Message is the message text, in Talk's markdown dialect.
	Message string
	// ReplyTo is the ID of the message being replied to, or zero for none. Talk
	// requires the parent to be a visible comment in the same conversation.
	ReplyTo int64
	// ThreadID targets an existing thread without quoting a specific message.
	// Talk rejects combining this with ReplyTo, so only one is ever sent.
	ThreadID int64
	// ReferenceID is an opaque client-chosen string Talk stores with the message
	// and echoes back, letting a client recognise its own messages.
	ReferenceID string
	// Silent suppresses notifications for the message.
	Silent bool
}

// form encodes the request as the urlencoded body Talk's chat endpoint expects.
func (r SendMessageRequest) form() url.Values {
	form := url.Values{"message": {r.Message}}
	if r.ReferenceID != "" {
		ref := r.ReferenceID
		if len(ref) > maxReferenceIDLength {
			ref = ref[:maxReferenceIDLength]
		}
		form.Set("referenceId", ref)
	}
	switch {
	case r.ReplyTo > 0:
		// A parent implies its thread, so replyTo alone is enough and avoids the
		// malformed-thread-parameters rejection.
		form.Set("replyTo", strconv.FormatInt(r.ReplyTo, 10))
	case r.ThreadID > 0:
		form.Set("threadId", strconv.FormatInt(r.ThreadID, 10))
	}
	if r.Silent {
		form.Set("silent", "true")
	}
	return form
}

// SendMessage posts a chat message to a conversation as the authenticated user
// and returns the message Talk created, whose ID the bridge needs in order to
// recognise the message when it comes back over the bot webhook.
func (c *Client) SendMessage(ctx context.Context, token string, req SendMessageRequest) (*Message, error) {
	var out Message
	_, err := c.requestJSON(ctx, http.MethodPost,
		SpreedAPI+"/api/v1/chat/"+url.PathEscape(token), nil, req.form(), &out)
	if err != nil {
		return nil, fmt.Errorf("send message to %s: %w", token, err)
	}
	if out.ID == 0 {
		return nil, ErrMessageNotReturned
	}
	if out.Token == "" {
		out.Token = token
	}
	return &out, nil
}

// IsBadRequest reports whether err is an OCS 400. Talk uses it for a message it
// understood but would not accept, such as a reply to a message that is not a
// valid parent, which the bridge retries without the reply relation.
func IsBadRequest(err error) bool {
	return hasStatus(err, http.StatusBadRequest)
}

// IsTooLarge reports whether err is an OCS 413, meaning the message exceeded
// MaxChatLength.
func IsTooLarge(err error) bool {
	return hasStatus(err, http.StatusRequestEntityTooLarge)
}

// IsForbidden reports whether err is an OCS 403, which Talk returns when the
// user may not post in the conversation at all.
func IsForbidden(err error) bool {
	return hasStatus(err, http.StatusForbidden)
}

// hasStatus reports whether err is an *Error carrying the given status, at
// either the OCS or the transport layer.
func hasStatus(err error, status int) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.StatusCode == status || e.HTTPStatus == status
}
