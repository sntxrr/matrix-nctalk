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

// EditMessage replaces the text of an existing message.
//
// Talk only permits this within 24 hours of the message being sent, only for
// your own messages unless you moderate the conversation, and only when the
// server advertises the edit-messages capability. It keeps the message ID, so
// nothing the bridge has recorded about the message changes.
func (c *Client) EditMessage(ctx context.Context, token string, messageID int64, message string) error {
	_, err := c.requestJSON(ctx, http.MethodPut, chatMessagePath(token, messageID),
		nil, url.Values{"message": {message}}, nil)
	if err != nil {
		return fmt.Errorf("edit message %d in %s: %w", messageID, token, err)
	}
	return nil
}

// DeleteMessage deletes a message.
//
// Talk only permits this within 6 hours, and it does not remove the message so
// much as replace its text with a "message deleted" placeholder, which is why
// the response carries a message rather than nothing.
func (c *Client) DeleteMessage(ctx context.Context, token string, messageID int64) error {
	_, err := c.requestJSON(ctx, http.MethodDelete, chatMessagePath(token, messageID), nil, nil, nil)
	if err != nil {
		return fmt.Errorf("delete message %d in %s: %w", messageID, token, err)
	}
	return nil
}

// SetReadMarker moves the authenticated user's read marker in a conversation.
//
// Talk tracks one marker per conversation rather than a receipt per message, so
// this is the whole of read-status support. Needs the chat-read-status
// capability.
func (c *Client) SetReadMarker(ctx context.Context, token string, lastReadMessage int64) error {
	form := url.Values{"lastReadMessage": {strconv.FormatInt(lastReadMessage, 10)}}
	_, err := c.requestJSON(ctx, http.MethodPost,
		SpreedAPI+"/api/v1/chat/"+url.PathEscape(token)+"/read", nil, form, nil)
	if err != nil {
		return fmt.Errorf("set read marker in %s: %w", token, err)
	}
	return nil
}

func chatMessagePath(token string, messageID int64) string {
	return fmt.Sprintf("%s/api/v1/chat/%s/%d", SpreedAPI, url.PathEscape(token), messageID)
}

// GetMessage returns a single message as the authenticated user sees it.
//
// This matters for rich objects: a file's `path` is resolved relative to
// whoever is asking, and the copy delivered over the bot webhook is resolved
// for nobody in particular, so it cannot be used to fetch the file.
func (c *Client) GetMessage(ctx context.Context, token string, messageID int64) (*Message, error) {
	var out []Message
	_, err := c.requestJSON(ctx, http.MethodGet,
		chatMessagePath(token, messageID)+"/context",
		url.Values{"limit": {"1"}}, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("get message %d in %s: %w", messageID, token, err)
	}
	for i := range out {
		if out[i].ID == messageID {
			return &out[i], nil
		}
	}
	return nil, fmt.Errorf("get message %d in %s: not in the returned context", messageID, token)
}

// ListMessages returns recent messages in a conversation, newest first.
func (c *Client) ListMessages(ctx context.Context, token string, limit int) ([]Message, error) {
	var out []Message
	_, err := c.requestJSON(ctx, http.MethodGet,
		SpreedAPI+"/api/v1/chat/"+url.PathEscape(token),
		url.Values{
			"lookIntoFuture": {"0"},
			"limit":          {strconv.Itoa(limit)},
		}, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("list messages in %s: %w", token, err)
	}
	return out, nil
}

// FindMessageByReference locates a recently sent message by the reference the
// client gave it.
//
// Some ways of posting — sharing a file, above all — create a chat message
// without reporting its ID. The reference is stored on the message, so a short
// look back over recent history recovers it.
func (c *Client) FindMessageByReference(ctx context.Context, token, referenceID string, limit int) (*Message, error) {
	if referenceID == "" {
		return nil, fmt.Errorf("no reference to look up")
	}
	if len(referenceID) > maxReferenceIDLength {
		// Talk truncated it on the way in, so search for what it actually stored.
		referenceID = referenceID[:maxReferenceIDLength]
	}
	messages, err := c.ListMessages(ctx, token, limit)
	if err != nil {
		return nil, err
	}
	for i := range messages {
		if messages[i].ReferenceID == referenceID {
			return &messages[i], nil
		}
	}
	return nil, fmt.Errorf("no message in %s carries reference %q", token, referenceID)
}

// IsBadRequest reports whether err is an OCS 400. Talk uses it for a message it
// understood but would not accept, such as a reply to a message that is not a
// valid parent, which the bridge retries without the reply relation.
func IsBadRequest(err error) bool {
	return hasStatus(err, http.StatusBadRequest)
}

// IsMethodNotAllowed reports whether err is an OCS 405, which Talk returns when
// the target of a delete is not a deletable message — a system message, or one
// that has already been deleted.
func IsMethodNotAllowed(err error) bool {
	return hasStatus(err, http.StatusMethodNotAllowed)
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
