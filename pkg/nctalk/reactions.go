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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Reactor is one participant's reaction to a message.
type Reactor struct {
	ActorType        string `json:"actorType"`
	ActorID          string `json:"actorId"`
	ActorDisplayName string `json:"actorDisplayName"`
	Timestamp        int64  `json:"timestamp"`
}

// ReactionList maps each emoji on a message to everyone who reacted with it.
type ReactionList map[string][]Reactor

// UnmarshalJSON accepts an object, an empty array, or null.
//
// Talk sends `{}` for a message with no reactions today, but its other maps
// come back as the PHP empty array `[]`, so both are accepted rather than
// letting a future change break every reaction sync.
func (r *ReactionList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("[]")) {
		*r = nil
		return nil
	}
	// Not the alias: that would recurse straight back into this method.
	var out map[string][]Reactor
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*r = out
	return nil
}

// ListReactions returns every reaction on a message, grouped by emoji.
//
// This is the only way to learn who reacted: Talk's Like and Undo webhooks name
// the author of the message rather than the person reacting to it, so the
// payload alone cannot be attributed.
func (c *Client) ListReactions(ctx context.Context, token string, messageID int64) (ReactionList, error) {
	var out ReactionList
	_, err := c.requestJSON(ctx, http.MethodGet, reactionPath(token, messageID), nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("list reactions on message %d in %s: %w", messageID, token, err)
	}
	return out, nil
}

// React adds a reaction to a message as the authenticated user.
//
// Talk models a reaction as the triple (message, actor, emoji) with no
// identifier of its own, so adding one that already exists is accepted as a
// no-op rather than rejected.
func (c *Client) React(ctx context.Context, token string, messageID int64, emoji string) error {
	_, err := c.requestJSON(ctx, http.MethodPost, reactionPath(token, messageID),
		nil, url.Values{"reaction": {emoji}}, nil)
	if err != nil {
		return fmt.Errorf("react to message %d in %s: %w", messageID, token, err)
	}
	return nil
}

// Unreact removes one of the authenticated user's own reactions. Talk has no
// way to remove somebody else's.
//
// The emoji goes in the query string rather than a request body. Nextcloud does
// parse urlencoded DELETE bodies, but Talk's own clients use the query string,
// and depending on the body parsing would be depending on the less-travelled
// path for no gain.
func (c *Client) Unreact(ctx context.Context, token string, messageID int64, emoji string) error {
	_, err := c.requestJSON(ctx, http.MethodDelete, reactionPath(token, messageID),
		url.Values{"reaction": {emoji}}, nil, nil)
	if err != nil {
		return fmt.Errorf("remove reaction from message %d in %s: %w", messageID, token, err)
	}
	return nil
}

func reactionPath(token string, messageID int64) string {
	return fmt.Sprintf("%s/api/v1/reaction/%s/%d", SpreedAPI, url.PathEscape(token), messageID)
}
