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
	"sort"
	"strconv"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/variationselector"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

var _ bridgev2.BackfillingNetworkAPI = (*NCTalkClient)(nil)

// maxBackfillPages bounds how many requests one batch will make. Talk caps a
// page at 200 messages, and a history dense with the system messages the bridge
// drops can yield a page with nothing worth sending, so a batch has to be
// allowed to page more than once — but not without end.
const maxBackfillPages = 20

// backfillReactionBudget bounds how many extra requests one batch spends on
// reaction detail. Talk's history reports only a count per emoji, so the people
// who reacted have to be fetched a message at a time, and a long history of
// reacted-to messages would otherwise turn one batch into hundreds of requests.
const backfillReactionBudget = 60

// FetchMessages implements bridgev2.BackfillingNetworkAPI.
//
// Talk's chat endpoint reads a conversation in one direction from a message ID,
// which maps onto both halves of bridgev2's backfill: forwards to catch up on
// what the webhook could not deliver, backwards to fill in history the bridge
// was never there for.
func (c *NCTalkClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	host, token, err := parsePortalID(params.Portal.ID)
	if err != nil {
		return nil, err
	}
	if host != c.host() {
		return nil, fmt.Errorf("portal %s belongs to a different Nextcloud server", params.Portal.ID)
	}
	if params.ThreadRoot != "" {
		// Talk's history endpoint cannot filter by thread, and the bridge never
		// asks for a thread backfill, so there is nothing to serve here.
		return &bridgev2.FetchMessagesResponse{}, nil
	}

	if params.Forward {
		return c.fetchForward(ctx, token, params)
	}
	return c.fetchBackward(ctx, token, params)
}

// fetchForward returns messages newer than what the portal already has, or the
// most recent history when it has none.
func (c *NCTalkClient) fetchForward(ctx context.Context, token string, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	anchor, hasAnchor := c.anchorMessageID(ctx, params.AnchorMessage)

	var collected []nctalk.Message
	var err error
	if hasAnchor {
		collected, err = c.pageForwards(ctx, token, anchor, params.Count)
	} else {
		// An empty portal has nothing to read forwards from, so the initial
		// backfill reads the newest page of history backwards and turns it round.
		collected, err = c.pageBackwards(ctx, token, 0, params.Count)
		reverseMessages(collected)
	}
	if err != nil {
		return nil, err
	}

	resp := &bridgev2.FetchMessagesResponse{
		Messages: c.convertBackfillBatch(ctx, params.Portal, collected),
		Forward:  true,
		// Webhook events are processed concurrently and Talk retries none of
		// them, so a message in this range may already be bridged, may be being
		// bridged right now, or may never arrive. Checking each one against the
		// database is the only way to avoid duplicating it.
		AggressiveDeduplication: params.AnchorMessage != nil,
	}
	if len(collected) > 0 {
		resp.MarkRead = c.alreadyReadThrough(ctx, token, newestMessageID(collected))
	}
	return resp, nil
}

// fetchBackward returns a page of history older than the portal's oldest
// message, continuing from the cursor of the previous call.
func (c *NCTalkClient) fetchBackward(ctx context.Context, token string, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	cursor, err := c.backwardCursor(ctx, params)
	if err != nil {
		return nil, err
	}

	log := zerolog.Ctx(ctx)
	var converted []*bridgev2.BackfillMessage
	for range maxBackfillPages {
		page, err := c.Client.GetHistory(ctx, nctalk.HistoryRequest{
			Token:              token,
			LastKnownMessageID: cursor,
			Limit:              params.Count,
		})
		if err != nil {
			return nil, err
		}
		// Talk reports the oldest message it considered, returned or not, so a
		// page whose every message was a reaction notice still moves the cursor.
		if page.LastGiven == 0 || (cursor > 0 && page.LastGiven >= cursor) {
			// Nothing older, or Talk stopped moving: the history ends here.
			return &bridgev2.FetchMessagesResponse{
				Messages: converted,
				Cursor:   formatCursor(cursor),
				HasMore:  false,
			}, nil
		}
		cursor = page.LastGiven

		reverseMessages(page.Messages)
		converted = append(c.convertBackfillBatch(ctx, params.Portal, page.Messages), converted...)
		if len(converted) > 0 {
			break
		}
		// Every message on that page was one the bridge does not send. Keep
		// paging rather than handing back an empty batch, which the queue would
		// treat as an unexplained gap.
		log.Debug().
			Int64("cursor", cursor).
			Msg("A page of Talk history held nothing to bridge, reading further back")
	}

	return &bridgev2.FetchMessagesResponse{
		Messages: converted,
		Cursor:   formatCursor(cursor),
		// Talk never says whether more history exists, so this claims there is
		// and lets the next call find the end. That costs one extra request per
		// conversation, once.
		HasMore: true,
	}, nil
}

// pageForwards collects up to count messages newer than the given ID.
func (c *NCTalkClient) pageForwards(ctx context.Context, token string, from int64, count int) ([]nctalk.Message, error) {
	var collected []nctalk.Message
	cursor := from
	for range maxBackfillPages {
		if len(collected) >= count {
			break
		}
		page, err := c.Client.GetHistory(ctx, nctalk.HistoryRequest{
			Token:              token,
			LastKnownMessageID: cursor,
			Limit:              count - len(collected),
			Future:             true,
		})
		if err != nil {
			if len(collected) == 0 {
				return nil, err
			}
			// Some catch-up beats none: deliver what was read and let the next
			// resync take another run at the rest.
			zerolog.Ctx(ctx).Warn().Err(err).
				Int("collected", len(collected)).
				Msg("Stopping forward backfill early after a failed page")
			break
		}
		if page.LastGiven <= cursor {
			break
		}
		cursor = page.LastGiven
		collected = append(collected, page.Messages...)
	}
	return collected, nil
}

// pageBackwards collects up to count messages older than the given ID, newest
// first. A zero from starts at the newest message in the conversation.
func (c *NCTalkClient) pageBackwards(ctx context.Context, token string, from int64, count int) ([]nctalk.Message, error) {
	var collected []nctalk.Message
	cursor := from
	for range maxBackfillPages {
		if len(collected) >= count {
			break
		}
		page, err := c.Client.GetHistory(ctx, nctalk.HistoryRequest{
			Token:              token,
			LastKnownMessageID: cursor,
			Limit:              count - len(collected),
		})
		if err != nil {
			if len(collected) == 0 {
				return nil, err
			}
			zerolog.Ctx(ctx).Warn().Err(err).
				Int("collected", len(collected)).
				Msg("Stopping initial backfill early after a failed page")
			break
		}
		if page.LastGiven == 0 || (cursor > 0 && page.LastGiven >= cursor) {
			break
		}
		cursor = page.LastGiven
		collected = append(collected, page.Messages...)
	}
	return collected, nil
}

// anchorMessageID resolves the Talk message ID of the message bridgev2 is
// paging from.
//
// A message the bridge relayed through the bot endpoint has no Talk message ID
// behind it at all, so it cannot be paged from; reading from the newest message
// instead leans on bridgev2 to discard what it already has.
func (c *NCTalkClient) anchorMessageID(ctx context.Context, anchor *database.Message) (int64, bool) {
	if anchor == nil {
		return 0, false
	}
	if _, _, messageID, err := parseMessageID(anchor.ID); err == nil {
		return messageID, true
	}
	zerolog.Ctx(ctx).Debug().
		Str("message_id", string(anchor.ID)).
		Msg("Backfill anchor carries no Talk message ID, reading from the newest message instead")
	return 0, false
}

// backwardCursor returns the Talk message ID to read history back from.
func (c *NCTalkClient) backwardCursor(ctx context.Context, params bridgev2.FetchMessagesParams) (int64, error) {
	if params.Cursor != "" {
		cursor, err := strconv.ParseInt(string(params.Cursor), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("malformed backfill cursor %q: %w", params.Cursor, err)
		}
		return cursor, nil
	}
	// No cursor yet, so this is the first batch: start from the oldest message
	// the portal already holds. A portal with none starts at the newest.
	cursor, _ := c.anchorMessageID(ctx, params.AnchorMessage)
	return cursor, nil
}

func formatCursor(id int64) networkid.PaginationCursor {
	if id <= 0 {
		return ""
	}
	return networkid.PaginationCursor(strconv.FormatInt(id, 10))
}

// convertBackfillBatch turns Talk messages, oldest first, into the batch
// bridgev2 sends, dropping the ones that have no place in a Matrix room.
func (c *NCTalkClient) convertBackfillBatch(ctx context.Context, portal *bridgev2.Portal, messages []nctalk.Message) []*bridgev2.BackfillMessage {
	log := zerolog.Ctx(ctx)
	out := make([]*bridgev2.BackfillMessage, 0, len(messages))
	reactionBudget := backfillReactionBudget
	var skippedReactions int

	for i := range messages {
		msg := &messages[i]
		converted, err := c.convertBackfillMessage(ctx, portal, msg, &reactionBudget, &skippedReactions)
		if err != nil {
			// One unconvertible message is not worth losing the batch over.
			log.Debug().Err(err).
				Int64("talk_message_id", msg.ID).
				Msg("Skipping a message while backfilling")
			continue
		}
		if converted != nil {
			out = append(out, converted)
		}
	}

	if skippedReactions > 0 {
		log.Info().
			Int("messages", skippedReactions).
			Int("budget", backfillReactionBudget).
			Msg("Backfilled some messages without their reactions to stay within the batch's request budget")
	}
	return out
}

// convertBackfillMessage converts one message from history, returning nil for
// one the bridge deliberately does not send.
func (c *NCTalkClient) convertBackfillMessage(
	ctx context.Context,
	portal *bridgev2.Portal,
	msg *nctalk.Message,
	reactionBudget, skippedReactions *int,
) (*bridgev2.BackfillMessage, error) {
	if msg.Deleted || msg.MessageType == nctalk.MessageTypeCommentDeleted {
		// Talk keeps a deleted message as a "message deleted" placeholder. That
		// is honest for a message the room already saw being deleted, but as
		// history it is noise standing in for something nobody can read.
		return nil, nil
	}
	if nctalk.IsRedundantSystemMessage(msg.SystemMessage) {
		// Reactions, edits and deletions are each bridged as themselves, so
		// their narration would double every one of them in the backfilled room.
		return nil, nil
	}
	if !isBridgeableActor(msg.ActorType) {
		return nil, nil
	}

	talkMsg := &talkMessage{
		Token:      msg.Token,
		MessageID:  msg.ID,
		ActorType:  msg.ActorType,
		ActorID:    msg.ActorID,
		ActorName:  msg.ActorDisplayName,
		Text:       msg.Message,
		Parameters: msg.MessageParameters,
		IsMarkdown: msg.MarkdownFlag,
		SystemType: msg.SystemMessage,
		// The chat endpoint resolves a file's path against the asking user's own
		// files, which is the form WebDAV accepts, so nothing needs re-fetching.
		ParamsResolved: true,
		PublishedAt:    time.Unix(msg.Timestamp, 0),
	}
	if msg.Parent != nil {
		talkMsg.ReplyToID = msg.Parent.ID
	}

	sender := c.eventSender(msg.ActorType, msg.ActorID)
	converted, err := c.convertMessage(ctx, portal, c.backfillIntent(ctx, portal, sender), talkMsg)
	if err != nil {
		return nil, err
	}

	return &bridgev2.BackfillMessage{
		ConvertedMessage: converted,
		Sender:           sender,
		ID:               makeMessageID(c.host(), msg.Token, msg.ID),
		Timestamp:        talkMsg.timestamp(),
		// Talk message IDs are a per-server monotonic sequence, so they order the
		// room the same way live messages do.
		StreamOrder: msg.ID,
		Reactions:   c.backfillReactions(ctx, msg, reactionBudget, skippedReactions),
	}, nil
}

// backfillIntent returns the Matrix identity a backfilled message is sent as,
// or nil when there is no bridge to ask — media conversion falls back to text
// rather than failing when that happens.
func (c *NCTalkClient) backfillIntent(ctx context.Context, portal *bridgev2.Portal, sender bridgev2.EventSender) bridgev2.MatrixAPI {
	if portal.Bridge == nil {
		return nil
	}
	intent, ok := portal.GetIntentFor(ctx, sender, c.UserLogin, bridgev2.RemoteEventMessage)
	if !ok {
		return nil
	}
	return intent
}

// backfillReactions fetches who reacted to a backfilled message.
//
// Talk's history carries only a count per emoji, so the reactors are a separate
// request per message. That is charged against a per-batch budget: a backfilled
// reaction is worth having, but not worth an unbounded number of requests.
func (c *NCTalkClient) backfillReactions(ctx context.Context, msg *nctalk.Message, budget, skipped *int) []*bridgev2.BackfillReaction {
	if len(msg.Reactions) == 0 {
		return nil
	}
	if *budget <= 0 {
		*skipped++
		return nil
	}
	*budget--

	list, err := c.Client.ListReactions(ctx, msg.Token, msg.ID)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).
			Int64("talk_message_id", msg.ID).
			Msg("Could not read the reactions on a backfilled message")
		return nil
	}

	out := make([]*bridgev2.BackfillReaction, 0, len(list))
	for emoji, reactors := range list {
		for _, reactor := range reactors {
			if !isBridgeableActor(reactor.ActorType) {
				continue
			}
			out = append(out, &bridgev2.BackfillReaction{
				Sender: c.eventSender(reactor.ActorType, reactor.ActorID),
				// As in the live path, the ID stays exactly what Talk holds while
				// only the displayed form is qualified, so a reaction the bridge
				// itself sent matches the row already written for it.
				EmojiID:   makeEmojiID(emoji),
				Emoji:     variationselector.Add(emoji),
				Timestamp: time.Unix(reactor.Timestamp, 0),
			})
		}
	}
	// Talk groups reactions by emoji in a map, so without this the same history
	// would backfill in a different order every time.
	sort.Slice(out, func(i, j int) bool {
		if out[i].EmojiID != out[j].EmojiID {
			return out[i].EmojiID < out[j].EmojiID
		}
		return out[i].Sender.Sender < out[j].Sender.Sender
	})
	return out
}

// alreadyReadThrough reports whether the user has already read up to the newest
// message of a batch in Talk, in which case the room should not arrive full of
// notifications for history they have seen.
func (c *NCTalkClient) alreadyReadThrough(ctx context.Context, token string, newestID int64) bool {
	if newestID <= 0 {
		return false
	}
	conv, err := c.Client.GetConversation(ctx, token)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).
			Str("token", token).
			Msg("Could not read the conversation's read marker, leaving backfilled history unread")
		return false
	}
	return conv.LastReadMessage >= newestID
}

// newestMessageID returns the highest Talk message ID in a batch, whatever
// order it is in.
func newestMessageID(messages []nctalk.Message) int64 {
	var newest int64
	for i := range messages {
		if messages[i].ID > newest {
			newest = messages[i].ID
		}
	}
	return newest
}

// reverseMessages flips a page of history in place, turning Talk's
// newest-first order into the oldest-first order backfill batches use.
func reverseMessages(messages []nctalk.Message) {
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
}
