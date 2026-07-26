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
	"maunium.net/go/mautrix/bridgev2/database"
)

// UserLoginMetadata is stored alongside each logged-in Nextcloud account.
type UserLoginMetadata struct {
	// ServerURL is the Nextcloud base URL, e.g. https://cloud.example.com
	ServerURL string `json:"server_url"`
	// Username is the canonical Nextcloud user ID.
	Username string `json:"username"`
	// AppPassword authenticates OCS calls made on this user's behalf.
	AppPassword string `json:"app_password"`
	// Features caches the server's Talk capability list so the bridge can
	// decide which endpoints are safe to call without refetching every time.
	Features []string `json:"features,omitempty"`
}

// PortalMetadata is stored alongside each bridged conversation.
type PortalMetadata struct {
	// ConversationType is the Talk room type, used to distinguish DMs from groups.
	ConversationType int `json:"conversation_type"`
	// BotEnabled records whether the bridge bot has been enabled in this
	// conversation, so the bridge does not retry the moderator-only call on
	// every portal update.
	BotEnabled bool `json:"bot_enabled"`
	// BotEnableFailed records that enabling the bot was attempted and rejected,
	// so the bridge can surface a single actionable notice instead of retrying.
	BotEnableFailed bool `json:"bot_enable_failed,omitempty"`
}

// GhostMetadata is stored alongside each Nextcloud user's Matrix ghost.
type GhostMetadata struct {
	// ActorType is the Talk actor type this ghost represents.
	ActorType string `json:"actor_type"`
}

// MessageMetadata is stored alongside each bridged message.
type MessageMetadata struct {
	// ReferenceID is the reference string the bridge sent to Talk, retained to
	// correlate the webhook echo of a message the bridge itself sent.
	ReferenceID string `json:"reference_id,omitempty"`
	// SentViaBot records that the message was relayed by the bot rather than
	// posted as a real Nextcloud user, which limits later edit and delete.
	SentViaBot bool `json:"sent_via_bot,omitempty"`
}

// GetDBMetaTypes implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		UserLogin: func() any { return &UserLoginMetadata{} },
		Portal:    func() any { return &PortalMetadata{} },
		Ghost:     func() any { return &GhostMetadata{} },
		Message:   func() any { return &MessageMetadata{} },
	}
}
