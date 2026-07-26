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
	"context"
	"errors"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

var _ bridgev2.ReactionHandlingNetworkAPI = (*NCTalkClient)(nil)

// errNotAnEmoji covers Matrix's custom image reactions, whose key is an mxc URI
// rather than an emoji. GetCapabilities already declines them, but a client may
// send one anyway.
var errNotAnEmoji = bridgev2.WrapErrorInStatus(errors.New("Nextcloud Talk only accepts emoji as reactions")).
	WithIsCertain(true).WithErrorAsMessage().WithSendNotice(false).
	WithErrorReason(event.MessageStatusUnsupported)

// PreHandleMatrixReaction implements bridgev2.ReactionHandlingNetworkAPI.
//
// Talk identifies a reaction by (message, actor, emoji) and places no limit on
// how many different emoji one person may add, so the emoji is the ID and there
// is no maximum to enforce.
//
// The key is passed through byte for byte rather than normalised, because Talk
// stores it verbatim and echoes it back over the webhook: any normalisation
// here would have to be applied identically on ingress, or the bridge's own
// reaction would come back looking like somebody else's.
func (c *NCTalkClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	emoji := msg.Content.RelatesTo.Key
	if emoji == "" || strings.HasPrefix(emoji, "mxc://") {
		return bridgev2.MatrixReactionPreResponse{}, errNotAnEmoji
	}
	return bridgev2.MatrixReactionPreResponse{
		SenderID: makeUserID(c.host(), nctalk.ActorUsers, c.meta().Username),
		Emoji:    emoji,
		EmojiID:  makeEmojiID(emoji),
	}, nil
}

// HandleMatrixReaction implements bridgev2.ReactionHandlingNetworkAPI.
func (c *NCTalkClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	token, messageID, err := c.talkTarget(msg.Portal, msg.TargetMessage)
	if err != nil {
		return nil, err
	}
	if err := c.Client.React(ctx, token, messageID, msg.PreHandleResp.Emoji); err != nil {
		return nil, c.wrapModifyError(ctx, err, "react to")
	}
	// Everything else on the row is filled in by the bridge from the
	// pre-handle response.
	return &database.Reaction{}, nil
}

// HandleMatrixReactionRemove implements bridgev2.ReactionHandlingNetworkAPI.
func (c *NCTalkClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	target := msg.TargetReaction
	token, messageID, err := c.talkTarget(msg.Portal, &database.Message{ID: target.MessageID})
	if err != nil {
		return err
	}
	// The ID is the exact string sent to Talk; Emoji is only a fallback for rows
	// written before this connector set one.
	emoji := string(target.EmojiID)
	if emoji == "" {
		emoji = target.Emoji
	}
	if err := c.Client.Unreact(ctx, token, messageID, emoji); err != nil {
		// Talk answers 404 when the reaction is already gone, which is the
		// desired end state, so there is nothing to report.
		if nctalk.IsNotFound(err) {
			return nil
		}
		return c.wrapModifyError(ctx, err, "remove the reaction from")
	}
	return nil
}
