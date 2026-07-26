package connector

import (
	"fmt"
	"strconv"
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// Network IDs are all prefixed with the Nextcloud host so that one bridge can
// serve several Nextcloud servers without collisions. Talk conversation tokens
// and message IDs are only unique within a single server.

// makeUserLoginID builds the ID for a logged-in Nextcloud account.
func makeUserLoginID(host, ncUserID string) networkid.UserLoginID {
	return networkid.UserLoginID(host + ":" + ncUserID)
}

// parseUserLoginID splits a login ID back into its host and Nextcloud user ID.
func parseUserLoginID(id networkid.UserLoginID) (host, ncUserID string, err error) {
	host, ncUserID, found := strings.Cut(string(id), ":")
	if !found || host == "" || ncUserID == "" {
		return "", "", fmt.Errorf("malformed user login ID %q", id)
	}
	return host, ncUserID, nil
}

// makeUserID builds the ghost ID for a Talk actor. The actor type is retained
// because Talk distinguishes users, guests, bots and federated users, and the
// same actor ID can exist under different types.
func makeUserID(host, actorType, actorID string) networkid.UserID {
	return networkid.UserID(host + ":" + actorType + ":" + actorID)
}

// parseUserID splits a ghost ID into host, actor type and actor ID.
func parseUserID(id networkid.UserID) (host, actorType, actorID string, err error) {
	parts := strings.SplitN(string(id), ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("malformed user ID %q", id)
	}
	return parts[0], parts[1], parts[2], nil
}

// makePortalID builds the portal ID for a Talk conversation.
func makePortalID(host, token string) networkid.PortalID {
	return networkid.PortalID(host + ":" + token)
}

// parsePortalID splits a portal ID into host and conversation token.
func parsePortalID(id networkid.PortalID) (host, token string, err error) {
	host, token, found := strings.Cut(string(id), ":")
	if !found || host == "" || token == "" {
		return "", "", fmt.Errorf("malformed portal ID %q", id)
	}
	return host, token, nil
}

// makePortalKey builds the portal key for a conversation.
//
// Receiver is deliberately left empty: Talk conversation tokens are global to
// the server and both sides of a one-to-one conversation see the same token, so
// every bridged user of a conversation should share one Matrix room rather than
// each getting a private copy.
func makePortalKey(host, token string) networkid.PortalKey {
	return networkid.PortalKey{ID: makePortalID(host, token)}
}

// makeMessageID builds the message ID for a Talk chat message.
func makeMessageID(host, token string, messageID int64) networkid.MessageID {
	return networkid.MessageID(fmt.Sprintf("%s:%s:%d", host, token, messageID))
}

// parseMessageID splits a message ID into host, conversation token and the
// numeric Talk message ID.
func parseMessageID(id networkid.MessageID) (host, token string, messageID int64, err error) {
	parts := strings.SplitN(string(id), ":", 3)
	if len(parts) != 3 {
		return "", "", 0, fmt.Errorf("malformed message ID %q", id)
	}
	messageID, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", 0, fmt.Errorf("malformed message ID %q: %w", id, err)
	}
	return parts[0], parts[1], messageID, nil
}

// makeEmojiID builds the reaction ID for a Talk reaction. Talk models
// reactions as (message, actor, emoji) with no separate identifier.
func makeEmojiID(emoji string) networkid.EmojiID {
	return networkid.EmojiID(emoji)
}

// parseTalkActor splits an Activity Streams actor ID such as `users/alice` or
// `bots/bot-3f9a…` into its actor type and actor ID.
func parseTalkActor(actorID string) (actorType, id string, err error) {
	actorType, id, found := strings.Cut(actorID, "/")
	if !found || actorType == "" || id == "" {
		return "", "", fmt.Errorf("malformed Talk actor ID %q", actorID)
	}
	return actorType, id, nil
}

// isBridgeableActor reports whether messages from this actor type should be
// bridged. Talk uses several actor types the bridge has no meaningful mapping
// for; those are dropped rather than producing broken ghosts.
func isBridgeableActor(actorType string) bool {
	switch actorType {
	case nctalk.ActorUsers, nctalk.ActorGuests, nctalk.ActorFederatedUsers,
		nctalk.ActorEmails, nctalk.ActorDeletedUsers, nctalk.ActorBots:
		return true
	}
	return false
}
