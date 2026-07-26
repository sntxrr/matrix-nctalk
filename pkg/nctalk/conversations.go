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
	"fmt"
	"net/http"
	"net/url"
)

// ListConversations returns every conversation the authenticated user is in.
func (c *Client) ListConversations(ctx context.Context) ([]Conversation, error) {
	var out []Conversation
	_, err := c.requestJSON(ctx, http.MethodGet, SpreedAPI+"/api/v4/room", nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	return out, nil
}

// GetConversation returns a single conversation by token.
func (c *Client) GetConversation(ctx context.Context, token string) (*Conversation, error) {
	var out Conversation
	_, err := c.requestJSON(ctx, http.MethodGet, SpreedAPI+"/api/v4/room/"+url.PathEscape(token), nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("get conversation %s: %w", token, err)
	}
	return &out, nil
}

// ListParticipants returns the participants of a conversation.
//
// includeStatus asks Talk to include user status data, which the bridge does
// not need; it is left off to keep the response small.
func (c *Client) ListParticipants(ctx context.Context, token string) ([]Participant, error) {
	var out []Participant
	_, err := c.requestJSON(ctx, http.MethodGet,
		SpreedAPI+"/api/v4/room/"+url.PathEscape(token)+"/participants", nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("list participants of %s: %w", token, err)
	}
	return out, nil
}

// Capabilities fetches the server's Talk capabilities.
func (c *Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	var out struct {
		Capabilities struct {
			Spreed Capabilities `json:"spreed"`
		} `json:"capabilities"`
	}
	_, err := c.requestJSON(ctx, http.MethodGet, "/ocs/v2.php/cloud/capabilities", nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("get capabilities: %w", err)
	}
	return &out.Capabilities.Spreed, nil
}

// UserDetails is the subset of the provisioning user payload used to populate
// Matrix ghost profiles.
type UserDetails struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayname"`
	Email       string `json:"email"`
	Enabled     bool   `json:"enabled"`
}

// GetUserDetails looks up a Nextcloud user's profile.
//
// The provisioning API normally requires admin rights, but every user may read
// their own record; for other users Talk's participant list is the fallback
// source of display names.
func (c *Client) GetUserDetails(ctx context.Context, userID string) (*UserDetails, error) {
	var out UserDetails
	_, err := c.requestJSON(ctx, http.MethodGet,
		"/ocs/v2.php/cloud/users/"+url.PathEscape(userID), nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("get user %s: %w", userID, err)
	}
	return &out, nil
}

// AvatarURL returns the URL of a user's avatar at the given size. Nextcloud
// serves a generated initials avatar when the user has not set one, so this
// never 404s for a valid user.
func (c *Client) AvatarURL(userID string, size int) string {
	return fmt.Sprintf("%s/avatar/%s/%d", c.BaseURL, url.PathEscape(userID), size)
}

// ConversationAvatarURL returns the URL of a conversation's avatar.
func (c *Client) ConversationAvatarURL(token string) string {
	return fmt.Sprintf("%s%s/api/v1/room/%s/avatar", c.BaseURL, SpreedAPI, url.PathEscape(token))
}
