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
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

// talkEdit is the data a Talk-side edit carries into the portal event loop.
type talkEdit struct {
	// SystemMessageID is the ID of the "message_edited" system message, which is
	// what distinguishes this edit from the echo of one made from Matrix.
	SystemMessageID int64
	// Message is the edited message in its new state.
	Message *talkMessage
	// LastEditTimestamp is when Talk recorded the edit, in Unix seconds.
	LastEditTimestamp int64
}

// handleMessageChange bridges an edit or deletion made in Talk.
//
// Talk reports both as a system message whose parent is the changed message,
// but the bot webhook does not carry the parent, so the system message is
// re-read through the chat API as this login. The parent in that answer is the
// changed message as it now stands: the new text of an edit, or the
// placeholder left by a deletion.
//
// Changes the bridge made itself from Matrix come back the same way. A
// deletion's echo is harmless, because bridgev2 has already dropped the
// message row and the removal finds no target. An edit's echo is caught in
// convertTalkEdit.
func (c *NCTalkClient) handleMessageChange(ctx context.Context, evt *nctalk.WebhookEvent, note *nctalk.Note, token string, receivedAt time.Time) error {
	systemID, err := note.MessageID()
	if err != nil {
		return err
	}
	sys, err := c.Client.GetMessage(ctx, token, systemID)
	if err != nil {
		return fmt.Errorf("read %s system message %d: %w", note.Name, systemID, err)
	}
	if sys.Parent == nil || sys.Parent.ID == 0 {
		zerolog.Ctx(ctx).Warn().
			Str("system_message", note.Name).
			Int64("talk_message_id", systemID).
			Msg("Talk did not say which message was changed, ignoring")
		return nil
	}
	target := sys.Parent
	ts := evt.Timestamp()
	if ts.IsZero() {
		ts = receivedAt
	}
	logContext := func(lc zerolog.Context) zerolog.Context {
		return lc.Str("talk_token", token).
			Int64("talk_message_id", target.ID).
			Int64("talk_system_message_id", systemID).
			Str("talk_actor", evt.Actor.ID)
	}

	switch note.Name {
	case nctalk.SystemMessageDeleted:
		c.events().QueueRemoteEvent(c.UserLogin, &simplevent.MessageRemove{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventMessageRemove,
				PortalKey: makePortalKey(c.host(), token),
				// No sender: bridgev2 then redacts as the message's original
				// sender where it can, and as the bridge bot otherwise. The ghost
				// of whoever deleted it may lack the power level to redact someone
				// else's message, whereas Talk has already allowed the deletion.
				Timestamp:  ts,
				LogContext: logContext,
			},
			TargetMessage: makeMessageID(c.host(), token, target.ID),
		})
	case nctalk.SystemMessageEdited:
		edit := &talkEdit{
			SystemMessageID:   systemID,
			LastEditTimestamp: target.LastEditTimestamp,
			Message: &talkMessage{
				Token:      token,
				MessageID:  target.ID,
				ActorType:  target.ActorType,
				ActorID:    target.ActorID,
				ActorName:  target.ActorDisplayName,
				Text:       target.Message,
				Parameters: target.MessageParameters,
				IsMarkdown: target.MarkdownFlag,
				SystemType: target.SystemMessage,
				// Read from the chat API as this login, like history.
				ParamsResolved: true,
				PublishedAt:    time.Unix(target.Timestamp, 0),
			},
		}
		c.events().QueueRemoteEvent(c.UserLogin, &simplevent.Message[*talkEdit]{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventEdit,
				PortalKey: makePortalKey(c.host(), token),
				// Talk shows an edited message under its original author even
				// when a moderator made the change, so Matrix should too.
				Sender:     bridgev2.EventSender{ForceEditOrigSender: true},
				Timestamp:  ts,
				LogContext: logContext,
			},
			Data:            edit,
			TargetMessage:   makeMessageID(c.host(), token, target.ID),
			ConvertEditFunc: c.convertTalkEdit,
		})
	}
	return nil
}

// convertTalkEdit turns a Talk-side edit into replacement content for the
// bridged message.
func (c *NCTalkClient) convertTalkEdit(
	ctx context.Context,
	portal *bridgev2.Portal,
	intent bridgev2.MatrixAPI,
	existing []*database.Message,
	edit *talkEdit,
) (*bridgev2.ConvertedEdit, error) {
	if len(existing) == 0 {
		return nil, bridgev2.ErrIgnoringRemoteEvent
	}
	if isEditEcho(existing[0], edit) {
		return nil, fmt.Errorf("%w: echo of an edit made from Matrix", bridgev2.ErrIgnoringRemoteEvent)
	}
	// An edit to a shared file changes only its caption. Re-converting it would
	// download and upload the whole file again to change a line of text, so it
	// is left alone.
	if fileParamKey(edit.Message.Parameters) != "" {
		return nil, fmt.Errorf("%w: caption edits are not bridged", bridgev2.ErrIgnoringRemoteEvent)
	}

	converted, err := c.convertMessage(ctx, portal, intent, edit.Message)
	if err != nil {
		return nil, err
	}
	if len(converted.Parts) == 0 {
		return nil, bridgev2.ErrIgnoringRemoteEvent
	}
	out := &bridgev2.ConvertedEdit{}
	for i, part := range converted.Parts {
		if i >= len(existing) {
			break
		}
		out.ModifiedParts = append(out.ModifiedParts, &bridgev2.ConvertedEditPart{
			Part:    existing[i],
			Type:    part.Type,
			Content: part.Content,
			Extra:   part.Extra,
		})
	}
	return out, nil
}

// isEditEcho reports whether a Talk-side edit is the webhook echo of an edit
// the bridge made from Matrix, as recorded by recordEditEcho.
//
// System message IDs only grow, so an edit at or below the recorded one is
// that echo or an older edit already overtaken by it. The timestamp fallback
// covers a server whose edit response did not name the system message.
func isEditEcho(target *database.Message, edit *talkEdit) bool {
	meta, ok := target.Metadata.(*MessageMetadata)
	if !ok {
		return false
	}
	if meta.EditEchoID > 0 {
		return edit.SystemMessageID <= meta.EditEchoID
	}
	return meta.EditEchoTS > 0 && edit.LastEditTimestamp > 0 && edit.LastEditTimestamp <= meta.EditEchoTS
}
