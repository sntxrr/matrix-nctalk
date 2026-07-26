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
	"bytes"
	"encoding/json"
)

// Actor types used by Talk for message senders and participants.
const (
	ActorUsers           = "users"
	ActorGuests          = "guests"
	ActorBots            = "bots"
	ActorFederatedUsers  = "federated_users"
	ActorEmails          = "emails"
	ActorCircles         = "circles"
	ActorGroups          = "groups"
	ActorDeletedUsers    = "deleted_users"
	ActorPhones          = "phones"
	ActorBridgedFromMisc = "bridged"
)

// Conversation types, from Talk's Room model.
const (
	RoomTypeOneToOne       = 1
	RoomTypeGroup          = 2
	RoomTypePublic         = 3
	RoomTypeChangelog      = 4
	RoomTypeOneToOneFormer = 5
	RoomTypeNoteToSelf     = 6
)

// Participant permission bits relevant to the bridge. Talk exposes moderator
// status via participantType rather than the permissions bitfield.
const (
	ParticipantTypeOwner     = 1
	ParticipantTypeModerator = 2
	ParticipantTypeUser      = 3
	ParticipantTypeGuest     = 4
	ParticipantTypeUserSelf  = 5
	ParticipantTypeGuestMod  = 6
)

// Conversation is a Talk conversation as returned by the room endpoints.
type Conversation struct {
	ID               int64  `json:"id"`
	Token            string `json:"token"`
	Type             int    `json:"type"`
	Name             string `json:"name"`
	DisplayName      string `json:"displayName"`
	Description      string `json:"description"`
	ParticipantType  int    `json:"participantType"`
	ObjectType       string `json:"objectType"`
	ObjectID         string `json:"objectId"`
	ReadOnly         int    `json:"readOnly"`
	LastActivity     int64  `json:"lastActivity"`
	LastReadMessage  int64  `json:"lastReadMessage"`
	UnreadMessages   int    `json:"unreadMessages"`
	IsFavorite       bool   `json:"isFavorite"`
	AvatarVersion    string `json:"avatarVersion"`
	IsCustomAvatar   bool   `json:"isCustomAvatar"`
	CanLeaveConv     bool   `json:"canLeaveConversation"`
	CanDeleteConv    bool   `json:"canDeleteConversation"`
	LastCommonRead   int64  `json:"lastCommonReadMessage"`
	MessageExpiresIn int    `json:"messageExpiration"`
}

// IsOneToOne reports whether this conversation is a direct chat, which the
// bridge maps to a Matrix DM.
func (c *Conversation) IsOneToOne() bool {
	return c.Type == RoomTypeOneToOne || c.Type == RoomTypeOneToOneFormer
}

// IsModerator reports whether the authenticated user can administer the
// conversation, which is required to enable the bridge bot in it.
func (c *Conversation) IsModerator() bool {
	switch c.ParticipantType {
	case ParticipantTypeOwner, ParticipantTypeModerator, ParticipantTypeGuestMod:
		return true
	}
	return false
}

// Bridgeable reports whether the bridge can enable its bot in this
// conversation. Talk's BotController rejects federated and former one-to-one
// conversations outright, and the changelog conversation is bridge noise.
func (c *Conversation) Bridgeable() bool {
	switch c.Type {
	case RoomTypeChangelog, RoomTypeOneToOneFormer:
		return false
	}
	return true
}

// Participant is a member of a conversation.
type Participant struct {
	AttendeeID      int64  `json:"attendeeId"`
	ActorType       string `json:"actorType"`
	ActorID         string `json:"actorId"`
	DisplayName     string `json:"displayName"`
	ParticipantType int    `json:"participantType"`
	LastPing        int64  `json:"lastPing"`
	InCall          int    `json:"inCall"`
	Permissions     int    `json:"permissions"`
	Status          string `json:"status"`
}

// IsModerator reports whether this participant can administer the conversation.
func (p *Participant) IsModerator() bool {
	switch p.ParticipantType {
	case ParticipantTypeOwner, ParticipantTypeModerator, ParticipantTypeGuestMod:
		return true
	}
	return false
}

// Message is a Talk chat message.
type Message struct {
	ID                int64          `json:"id"`
	Token             string         `json:"token"`
	ActorType         string         `json:"actorType"`
	ActorID           string         `json:"actorId"`
	ActorDisplayName  string         `json:"actorDisplayName"`
	Timestamp         int64          `json:"timestamp"`
	Message           string         `json:"message"`
	MessageType       string         `json:"messageType"`
	MessageParameters MessageParams  `json:"messageParameters"`
	SystemMessage     string         `json:"systemMessage"`
	Deleted           bool           `json:"deleted"`
	ExpirationTime    int64          `json:"expirationTimestamp"`
	ReferenceID       string         `json:"referenceId"`
	Reactions         map[string]int `json:"reactions"`
	Parent            *Message       `json:"parent"`
	LastEditTimestamp int64          `json:"lastEditTimestamp"`
	LastEditActorID   string         `json:"lastEditActorId"`
	LastEditActorType string         `json:"lastEditActorType"`
	Silent            bool           `json:"silent"`
	MarkdownFlag      bool           `json:"markdown"`
	ThreadID          int64          `json:"threadId"`
}

// Message types Talk reports in the messageType field.
const (
	MessageTypeComment        = "comment"
	MessageTypeCommentDeleted = "comment_deleted"
	MessageTypeSystem         = "system"
	MessageTypeCommand        = "command"
	MessageTypeVoice          = "voice-message"
	MessageTypeRecordAudio    = "record-audio"
	MessageTypeRecordVideo    = "record-video"
)

// MessageParams is a Talk Rich Object String parameter map.
//
// It exists to absorb a PHP encoding quirk: an empty associative array encodes
// as the JSON array `[]` rather than the object `{}`, so Talk sends `[]` for
// every message that carries no rich objects — which is most of them. Decoding
// that straight into a map fails, so the common case would break.
type MessageParams map[string]MessageParam

// UnmarshalJSON accepts an object, an empty array, or null.
func (p *MessageParams) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("[]")) {
		*p = nil
		return nil
	}
	// Not the alias: that would recurse straight back into this method.
	var out map[string]MessageParam
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*p = out
	return nil
}

// MessageParam is one entry of a Rich Object String parameter map. The union of
// fields across object types is wide; only those the bridge consumes are named.
type MessageParam struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`

	// File objects.
	Path     string `json:"path"`
	Link     string `json:"link"`
	MimeType string `json:"mimetype"`
	Size     string `json:"size"`
	Width    string `json:"width"`
	Height   string `json:"height"`
	PreviewA string `json:"preview-available"`
	Etag     string `json:"etag"`
	// Mention objects reuse ID/Name; server is set for federated mentions.
	Server string `json:"server"`
}

// Rich object types the bridge special-cases.
const (
	ParamTypeFile           = "file"
	ParamTypeUser           = "user"
	ParamTypeCall           = "call"
	ParamTypeGuest          = "guest"
	ParamTypeUserGroup      = "user-group"
	ParamTypeTalkPoll       = "talk-poll"
	ParamTypeDeckCard       = "deck-card"
	ParamTypeGeoLocation    = "geo-location"
	ParamTypeHighlight      = "highlight"
	ParamTypeFederatedUser  = "federated_user"
	ParamTypeCircle         = "circle"
	ParamTypeTalkAttachment = "talk-attachment"
)

// System message types the bridge suppresses, because each one narrates
// something it already bridges as a first-class event. Left in, they would
// double every reaction, edit and deletion with a notice describing it.
const (
	SystemReaction        = "reaction"
	SystemReactionDeleted = "reaction_deleted"
	SystemReactionRevoked = "reaction_revoked"
	SystemMessageDeleted  = "message_deleted"
	SystemMessageEdited   = "message_edited"
)

// IsRedundantSystemMessage reports whether a system message only restates an
// event the bridge already delivers by other means.
func IsRedundantSystemMessage(systemType string) bool {
	switch systemType {
	case SystemReaction, SystemReactionDeleted, SystemReactionRevoked,
		SystemMessageDeleted, SystemMessageEdited:
		return true
	}
	return false
}

// Capabilities is the subset of the Talk capability payload the bridge needs to
// decide which features to advertise and which endpoints are safe to call.
type Capabilities struct {
	Features []string       `json:"features"`
	Config   map[string]any `json:"config"`
}

// Has reports whether the server advertises the named Talk feature, e.g.
// "bots-v1", "edit-messages", "delete-messages", "reactions".
func (c *Capabilities) Has(feature string) bool {
	for _, f := range c.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// Talk capability names the bridge checks.
const (
	CapBotsV1         = "bots-v1"
	CapReactions      = "reactions"
	CapEditMessages   = "edit-messages"
	CapDeleteMessages = "delete-messages"
	CapChatReadStatus = "chat-read-status"
	CapTyping         = "typing-privacy"
	CapSilentSend     = "silent-send"
	CapThreads        = "threads"
)
