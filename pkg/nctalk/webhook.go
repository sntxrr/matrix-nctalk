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
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Webhook activity types Talk sends to bots.
const (
	// ActivityCreate is a new chat message.
	ActivityCreate = "Create"
	// ActivityActivity is a system message (call started, user added, ...).
	// This one is absent from the published bot documentation but is emitted by
	// BotService::afterSystemMessageSent.
	ActivityActivity = "Activity"
	// ActivityLike is a reaction being added.
	ActivityLike = "Like"
	// ActivityUndo is a reaction being removed; its object is the original Like.
	ActivityUndo = "Undo"
	// ActivityJoin is the bot being enabled in a conversation.
	ActivityJoin = "Join"
	// ActivityLeave is the bot being removed from a conversation.
	ActivityLeave = "Leave"
)

// Webhook headers Talk sets on every bot request.
const (
	HeaderRandom    = "X-Nextcloud-Talk-Random"
	HeaderSignature = "X-Nextcloud-Talk-Signature"
	// HeaderBackend carries the Nextcloud base URL with a trailing slash, which
	// is how the bridge tells multiple Nextcloud servers apart.
	HeaderBackend = "X-Nextcloud-Talk-Backend"
)

// WebhookEvent is the Activity Streams 2.0 envelope Talk posts to bots.
//
// Object is left raw because its shape depends on Type: a Note for messages,
// reactions and system messages, a Collection for Join and Leave, and a nested
// Like activity for Undo.
type WebhookEvent struct {
	Type   string          `json:"type"`
	Actor  Actor           `json:"actor"`
	Object json.RawMessage `json:"object"`
	Target Collection      `json:"target"`
	// Content holds the emoji on Like activities.
	Content   string `json:"content"`
	Published string `json:"published"`
}

// Actor identifies who performed the activity.
type Actor struct {
	Type string `json:"type"`
	// ID is prefixed with the actor type, e.g. `users/alice` or `bots/bot-3f9a`.
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Collection identifies a conversation. ID is the conversation token.
type Collection struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Note is a chat message or system message inside an activity.
type Note struct {
	Type string `json:"type"`
	// ID is the numeric Talk message ID, sent as a string.
	ID string `json:"id"`
	// Name is "message" for chat messages, or the raw system message type
	// (call_started, user_added, ...) for system messages.
	Name string `json:"name"`
	// Content is a JSON *string* holding {"message": ..., "parameters": {...}}.
	Content string `json:"content"`
	// MediaType is text/markdown when the message should be parsed as markdown.
	MediaType string `json:"mediaType"`
	InReplyTo *struct {
		Type   string `json:"type"`
		Actor  Actor  `json:"actor"`
		Object Note   `json:"object"`
	} `json:"inReplyTo"`
}

// NoteContent is the decoded payload of Note.Content.
type NoteContent struct {
	Message    string        `json:"message"`
	Parameters MessageParams `json:"parameters"`
}

// MessageID parses the note's message ID.
func (n *Note) MessageID() (int64, error) {
	id, err := strconv.ParseInt(n.ID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed message ID %q: %w", n.ID, err)
	}
	return id, nil
}

// IsMarkdown reports whether the message body should be rendered as markdown.
func (n *Note) IsMarkdown() bool {
	return n.MediaType == "text/markdown"
}

// DecodeContent parses the nested JSON string in Content.
func (n *Note) DecodeContent() (*NoteContent, error) {
	var out NoteContent
	if n.Content == "" {
		return &out, nil
	}
	if err := json.Unmarshal([]byte(n.Content), &out); err != nil {
		return nil, fmt.Errorf("decode note content: %w", err)
	}
	return &out, nil
}

// Note decodes the activity's object as a Note. Valid for Create, Activity and
// Like activities.
func (e *WebhookEvent) Note() (*Note, error) {
	var note Note
	if err := json.Unmarshal(e.Object, &note); err != nil {
		return nil, fmt.Errorf("decode %s object as note: %w", e.Type, err)
	}
	return &note, nil
}

// UndoneLike decodes the activity's object as the Like being undone. Valid for
// Undo activities.
func (e *WebhookEvent) UndoneLike() (*WebhookEvent, error) {
	var like WebhookEvent
	if err := json.Unmarshal(e.Object, &like); err != nil {
		return nil, fmt.Errorf("decode undo object as like: %w", err)
	}
	return &like, nil
}

// Conversation decodes the activity's object as a conversation. Valid for Join
// and Leave activities, where the target field is not populated.
func (e *WebhookEvent) Conversation() (*Collection, error) {
	var coll Collection
	if err := json.Unmarshal(e.Object, &coll); err != nil {
		return nil, fmt.Errorf("decode %s object as collection: %w", e.Type, err)
	}
	return &coll, nil
}

// Token returns the conversation token the activity belongs to.
//
// Most activities carry it in target; Join and Leave carry it in object.
func (e *WebhookEvent) Token() (string, error) {
	if e.Target.ID != "" {
		return e.Target.ID, nil
	}
	if e.Type == ActivityJoin || e.Type == ActivityLeave {
		conv, err := e.Conversation()
		if err != nil {
			return "", err
		}
		if conv.ID != "" {
			return conv.ID, nil
		}
	}
	return "", fmt.Errorf("%s activity has no conversation token", e.Type)
}

// Timestamp parses the published time, falling back to the zero time so callers
// can substitute the receive time.
func (e *WebhookEvent) Timestamp() time.Time {
	if e.Published == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339, e.Published)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// ParseWebhookEvent decodes a verified webhook body.
func ParseWebhookEvent(body []byte) (*WebhookEvent, error) {
	var evt WebhookEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return nil, fmt.Errorf("decode webhook body: %w", err)
	}
	if evt.Type == "" {
		return nil, fmt.Errorf("webhook body has no activity type")
	}
	return &evt, nil
}
