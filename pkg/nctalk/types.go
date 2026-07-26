package nctalk

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
	ID                int64                   `json:"id"`
	Token             string                  `json:"token"`
	ActorType         string                  `json:"actorType"`
	ActorID           string                  `json:"actorId"`
	ActorDisplayName  string                  `json:"actorDisplayName"`
	Timestamp         int64                   `json:"timestamp"`
	Message           string                  `json:"message"`
	MessageType       string                  `json:"messageType"`
	MessageParameters map[string]MessageParam `json:"messageParameters"`
	SystemMessage     string                  `json:"systemMessage"`
	Deleted           bool                    `json:"deleted"`
	ExpirationTime    int64                   `json:"expirationTimestamp"`
	ReferenceID       string                  `json:"referenceId"`
	Reactions         map[string]int          `json:"reactions"`
	Parent            *Message                `json:"parent"`
	LastEditTimestamp int64                   `json:"lastEditTimestamp"`
	LastEditActorID   string                  `json:"lastEditActorId"`
	LastEditActorType string                  `json:"lastEditActorType"`
	Silent            bool                    `json:"silent"`
	MarkdownFlag      bool                    `json:"markdown"`
	ThreadID          int64                   `json:"threadId"`
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
