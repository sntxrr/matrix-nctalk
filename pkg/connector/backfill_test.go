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
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

const backfillToken = "abc123token"

// chatRequest is one call the connector made to the chat endpoint.
type chatRequest struct {
	LastKnown int64
	Limit     int
	Future    bool
}

// fakeHistory serves a conversation's messages from a fixed list, the way Talk
// does: newest first going back, oldest first going forward, and a 304 with no
// body once there is nothing left on that side of the cursor.
type fakeHistory struct {
	// messages are in chronological order, oldest first.
	messages []nctalk.Message
	// withheld are message IDs Talk counts towards the page and the cursor but
	// does not return, as it does for anything expired or invisible to the
	// reader. A page of nothing but these comes back as an empty list with a
	// cursor, not as the 304 that means the history has ended.
	withheld map[int64]bool
	// reactions answers the reaction endpoint, keyed by message ID.
	reactions map[int64]nctalk.ReactionList
	// conversation is what the room endpoint reports.
	conversation map[string]any

	mu       sync.Mutex
	requests []chatRequest
	// reactionCalls counts how often the reactors of a message were fetched.
	reactionCalls int
}

func (f *fakeHistory) recorded() []chatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chatRequest(nil), f.requests...)
}

func (f *fakeHistory) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/context"):
			// History read as this login already resolves a file's path against
			// their own files, so a backfill should never need to look one up.
			t.Errorf("backfill asked to resolve %s, which history had already answered", r.URL.Path)
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/reaction/"):
			f.mu.Lock()
			f.reactionCalls++
			f.mu.Unlock()
			id, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
			list := f.reactions[id]
			if list == nil {
				list = nctalk.ReactionList{}
			}
			writeOCS(t, w, list)
		case strings.Contains(r.URL.Path, "/chat/"):
			f.serveChat(t, w, r)
		default:
			conv := f.conversation
			if conv == nil {
				conv = map[string]any{"token": backfillToken, "type": nctalk.RoomTypeGroup}
			}
			writeOCS(t, w, conv)
		}
	}
}

func (f *fakeHistory) serveChat(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	query := r.URL.Query()
	lastKnown, _ := strconv.ParseInt(query.Get("lastKnownMessageId"), 10, 64)
	limit, _ := strconv.Atoi(query.Get("limit"))
	future := query.Get("lookIntoFuture") == "1"

	f.mu.Lock()
	f.requests = append(f.requests, chatRequest{LastKnown: lastKnown, Limit: limit, Future: future})
	f.mu.Unlock()

	// Talk selects the page first and filters it afterwards, so the cursor comes
	// from what it considered rather than from what it sends back.
	var considered []nctalk.Message
	if future {
		for _, msg := range f.messages {
			if msg.ID > lastKnown && len(considered) < limit {
				considered = append(considered, msg)
			}
		}
	} else {
		for i := len(f.messages) - 1; i >= 0 && len(considered) < limit; i-- {
			if lastKnown == 0 || f.messages[i].ID < lastKnown {
				considered = append(considered, f.messages[i])
			}
		}
	}

	// Only an empty selection is the end of the history. A selection that was
	// entirely withheld is a 200 carrying an empty list and a live cursor.
	if len(considered) == 0 {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	visible := []nctalk.Message{}
	for _, msg := range considered {
		if !f.withheld[msg.ID] {
			visible = append(visible, msg)
		}
	}
	w.Header().Set(nctalk.HeaderChatLastGiven, strconv.FormatInt(considered[len(considered)-1].ID, 10))
	writeOCS(t, w, visible)
}

// newBackfillClient wires a client to a conversation with the given history.
func newBackfillClient(t *testing.T, history *fakeHistory) (*NCTalkClient, *bridgev2.Portal) {
	t.Helper()
	url, _ := newOCSServer(t, history.handler(t))
	client := newTestClient(t, url, "alice", botConfig())
	return client, newTestPortal(client.host(), backfillToken)
}

// comment builds a plain chat message as the chat endpoint returns it.
func comment(id int64, actor, text string) nctalk.Message {
	return nctalk.Message{
		ID:           id,
		Token:        backfillToken,
		ActorType:    nctalk.ActorUsers,
		ActorID:      actor,
		MessageType:  nctalk.MessageTypeComment,
		Message:      text,
		MarkdownFlag: true,
		Timestamp:    1700000000 + id,
	}
}

// systemMessage builds a system message of the given type.
func systemMessage(id int64, systemType, text string) nctalk.Message {
	return nctalk.Message{
		ID:            id,
		Token:         backfillToken,
		ActorType:     nctalk.ActorUsers,
		ActorID:       "alice",
		MessageType:   nctalk.MessageTypeSystem,
		SystemMessage: systemType,
		Message:       text,
		Timestamp:     1700000000 + id,
	}
}

// bodies returns the plain text of each part of a backfilled batch.
func bodies(messages []*bridgev2.BackfillMessage) []string {
	out := make([]string, 0, len(messages))
	for _, msg := range messages {
		for _, part := range msg.Parts {
			out = append(out, part.Content.Body)
		}
	}
	return out
}

func TestFetchMessagesInitialReadsNewestHistoryInOrder(t *testing.T) {
	history := &fakeHistory{messages: []nctalk.Message{
		comment(10, "bob", "oldest"),
		comment(11, "alice", "middle"),
		comment(12, "bob", "newest"),
	}}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:  portal,
		Forward: true,
		Count:   2,
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	if !resp.Forward {
		t.Error("the first batch in an empty room is a forward backfill")
	}
	// bridgev2 requires chronological order, but Talk hands history back newest
	// first, so the page has to be turned round.
	if got := bodies(resp.Messages); len(got) != 2 || got[0] != "middle" || got[1] != "newest" {
		t.Errorf("bodies = %v, want the newest two oldest-first", got)
	}
	if resp.Messages[0].StreamOrder != 11 {
		t.Errorf("StreamOrder = %d, want the Talk message ID", resp.Messages[0].StreamOrder)
	}
	if resp.Messages[0].ID != makeMessageID(client.host(), backfillToken, 11) {
		t.Errorf("message ID = %q", resp.Messages[0].ID)
	}
	// Nothing was in the room yet, so there is nothing to deduplicate against.
	if resp.AggressiveDeduplication {
		t.Error("an empty room needs no per-message duplicate check")
	}
}

func TestFetchMessagesCatchesUpFromTheAnchor(t *testing.T) {
	history := &fakeHistory{messages: []nctalk.Message{
		comment(10, "bob", "already bridged"),
		comment(11, "alice", "missed one"),
		comment(12, "bob", "missed two"),
	}}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:        portal,
		Forward:       true,
		Count:         10,
		AnchorMessage: &database.Message{ID: makeMessageID(client.host(), backfillToken, 10)},
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	if got := bodies(resp.Messages); len(got) != 2 || got[0] != "missed one" {
		t.Errorf("bodies = %v, want only what came after the anchor", got)
	}
	// Webhook delivery is concurrent and unretried, so a message in this range
	// may already have been bridged by the live path.
	if !resp.AggressiveDeduplication {
		t.Error("catching up on missed messages needs a per-message duplicate check")
	}

	requests := history.recorded()
	if len(requests) == 0 || !requests[0].Future || requests[0].LastKnown != 10 {
		t.Errorf("first request = %+v, want a forward read from message 10", requests)
	}
}

func TestFetchMessagesPagesForwardsToTheRequestedCount(t *testing.T) {
	var messages []nctalk.Message
	for id := int64(1); id <= 6; id++ {
		messages = append(messages, comment(id, "bob", fmt.Sprintf("message %d", id)))
	}
	// Talk serves at most one page per call, so reaching the count takes several.
	history := &fakeHistory{messages: messages}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:        portal,
		Forward:       true,
		Count:         5,
		AnchorMessage: &database.Message{ID: makeMessageID(client.host(), backfillToken, 1)},
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	if len(resp.Messages) != 5 {
		t.Fatalf("got %d messages, want the 5 that were asked for", len(resp.Messages))
	}
	if got := bodies(resp.Messages); got[0] != "message 2" || got[4] != "message 6" {
		t.Errorf("bodies = %v", got)
	}
}

func TestFetchMessagesBackwardsPagesAndReportsACursor(t *testing.T) {
	history := &fakeHistory{messages: []nctalk.Message{
		comment(10, "bob", "oldest"),
		comment(11, "alice", "middle"),
		comment(12, "bob", "newest"),
	}}
	client, portal := newBackfillClient(t, history)

	first, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:        portal,
		Count:         2,
		AnchorMessage: &database.Message{ID: makeMessageID(client.host(), backfillToken, 12)},
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	if got := bodies(first.Messages); len(got) != 2 || got[0] != "oldest" || got[1] != "middle" {
		t.Errorf("bodies = %v, want the two older messages oldest-first", got)
	}
	if first.Cursor != "10" {
		t.Errorf("cursor = %q, want the oldest message of the batch", first.Cursor)
	}
	// Talk never says whether more history exists, so the end is found by asking.
	if !first.HasMore {
		t.Error("HasMore should stay set until a page comes back empty")
	}

	second, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal: portal,
		Count:  2,
		Cursor: first.Cursor,
	})
	if err != nil {
		t.Fatalf("second FetchMessages failed: %v", err)
	}
	if len(second.Messages) != 0 {
		t.Errorf("got %d messages past the start of the conversation", len(second.Messages))
	}
	if second.HasMore {
		t.Error("HasMore should be clear once Talk has nothing older")
	}
}

func TestFetchMessagesBackwardsRejectsAMalformedCursor(t *testing.T) {
	client, portal := newBackfillClient(t, &fakeHistory{})

	_, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal: portal,
		Count:  10,
		Cursor: "not-a-message-id",
	})
	if err == nil {
		t.Fatal("expected an error for a cursor that is not a Talk message ID")
	}
}

func TestFetchMessagesBackwardsKeepsPagingPastUnbridgeableMessages(t *testing.T) {
	// A conversation where reactions outnumber messages is ordinary in Talk, and
	// a whole page of them would otherwise look to the queue like a gap.
	history := &fakeHistory{messages: []nctalk.Message{
		comment(10, "bob", "worth bridging"),
		systemMessage(11, nctalk.SystemReaction, "👍"),
		systemMessage(12, nctalk.SystemReaction, "🔥"),
		systemMessage(13, nctalk.SystemReactionRevoked, "You deleted a reaction"),
	}}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal: portal,
		Count:  2,
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	if got := bodies(resp.Messages); len(got) != 1 || got[0] != "worth bridging" {
		t.Errorf("bodies = %v, want the one real message from further back", got)
	}
	if len(history.recorded()) < 2 {
		t.Errorf("made %d requests, want more than one page to be read", len(history.recorded()))
	}
}

func TestFetchMessagesBackwardsPagesPastMessagesTalkWithheld(t *testing.T) {
	// Talk's cursor counts messages it did not return — expired, or invisible to
	// the reader — so a page can carry a live cursor and an empty list. Stopping
	// at the first empty page would lose everything older than it.
	history := &fakeHistory{
		messages: []nctalk.Message{
			comment(10, "bob", "older than the gap"),
			comment(11, "alice", "withheld one"),
			comment(12, "bob", "withheld two"),
		},
		withheld: map[int64]bool{11: true, 12: true},
	}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal: portal,
		Count:  2,
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	if got := bodies(resp.Messages); len(got) != 1 || got[0] != "older than the gap" {
		t.Fatalf("bodies = %v, want the message beyond the withheld page", got)
	}

	requests := history.recorded()
	if len(requests) != 2 {
		t.Fatalf("made %d requests, want a second one following the cursor", len(requests))
	}
	// The second request must follow the cursor Talk gave, not the last message
	// it actually returned — there was none.
	if requests[1].LastKnown != 11 {
		t.Errorf("second request read from %d, want the cursor 11", requests[1].LastKnown)
	}
}

func TestFetchMessagesForwardsPagesPastMessagesTalkWithheld(t *testing.T) {
	// The same trap in the catch-up direction, where giving up early would leave
	// the newest messages permanently unbridged.
	history := &fakeHistory{
		messages: []nctalk.Message{
			comment(10, "bob", "the anchor"),
			comment(11, "alice", "withheld one"),
			comment(12, "bob", "withheld two"),
			comment(13, "alice", "newer than the gap"),
		},
		withheld: map[int64]bool{11: true, 12: true},
	}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:        portal,
		Forward:       true,
		Count:         1,
		AnchorMessage: &database.Message{ID: makeMessageID(client.host(), backfillToken, 10)},
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	if got := bodies(resp.Messages); len(got) != 1 || got[0] != "newer than the gap" {
		t.Fatalf("bodies = %v, want the message beyond the withheld pages", got)
	}
	if len(history.recorded()) != 3 {
		t.Errorf("made %d requests, want one per withheld page plus the one that found it",
			len(history.recorded()))
	}
}

func TestFetchMessagesDropsWhatHasNoPlaceInMatrix(t *testing.T) {
	history := &fakeHistory{messages: []nctalk.Message{
		comment(10, "bob", "a real message"),
		// Each of these narrates something the bridge sends as itself.
		systemMessage(11, nctalk.SystemReaction, "👍"),
		systemMessage(12, nctalk.SystemMessageEdited, "You edited a message"),
		systemMessage(13, nctalk.SystemMessageDeleted, "You deleted a message"),
		// Talk keeps a deleted message as a placeholder; as history it stands in
		// for something nobody can read.
		{
			ID: 14, Token: backfillToken, ActorType: nctalk.ActorUsers, ActorID: "alice",
			MessageType: nctalk.MessageTypeCommentDeleted, Deleted: true,
			Message: "Message deleted by you", Timestamp: 1700000014,
		},
		// A conversation-level system message has no first-class equivalent, so
		// it stays as a notice.
		systemMessage(15, "description_set", "You set the description"),
		// Actor types with no Matrix mapping are dropped rather than producing
		// broken ghosts.
		{
			ID: 16, Token: backfillToken, ActorType: nctalk.ActorCircles, ActorID: "team",
			MessageType: nctalk.MessageTypeComment, Message: "from a circle", Timestamp: 1700000016,
		},
	}}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:  portal,
		Forward: true,
		Count:   20,
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	got := bodies(resp.Messages)
	if len(got) != 2 || got[0] != "a real message" || got[1] != "You set the description" {
		t.Fatalf("bodies = %v, want the message and the conversation notice only", got)
	}
	if resp.Messages[1].Parts[0].Content.MsgType != event.MsgNotice {
		t.Error("a system message should be backfilled as a notice")
	}
}

func TestFetchMessagesCarriesRepliesAndReactions(t *testing.T) {
	history := &fakeHistory{
		messages: []nctalk.Message{
			comment(10, "bob", "the parent"),
			func() nctalk.Message {
				msg := comment(11, "alice", "the reply")
				msg.Parent = &nctalk.Message{ID: 10}
				msg.Reactions = map[string]int{"👍": 2}
				return msg
			}(),
		},
		reactions: map[int64]nctalk.ReactionList{
			11: {"👍": {
				{ActorType: nctalk.ActorUsers, ActorID: "bob", Timestamp: 1700000100},
				{ActorType: nctalk.ActorUsers, ActorID: "alice", Timestamp: 1700000101},
			}},
		},
	}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:  portal,
		Forward: true,
		Count:   10,
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(resp.Messages))
	}

	reply := resp.Messages[1]
	if reply.ReplyTo == nil || reply.ReplyTo.MessageID != makeMessageID(client.host(), backfillToken, 10) {
		t.Errorf("reply relation = %+v, want it pointing at message 10", reply.ReplyTo)
	}
	if len(reply.Reactions) != 2 {
		t.Fatalf("got %d reactions, want both reactors", len(reply.Reactions))
	}
	// Talk's history reports only a count per emoji, so the reactors come from a
	// separate call — and only for messages that report any.
	if history.reactionCalls != 1 {
		t.Errorf("fetched reactors %d times, want only for the message that has some", history.reactionCalls)
	}
	// The stored ID stays exactly what Talk holds so a reaction the bridge itself
	// sent matches the row already written for it.
	if reply.Reactions[0].EmojiID != makeEmojiID("👍") {
		t.Errorf("emoji ID = %q, want the bare emoji", reply.Reactions[0].EmojiID)
	}
	if reply.Reactions[0].Sender.Sender != makeUserID(client.host(), nctalk.ActorUsers, "alice") {
		t.Errorf("first reactor = %q, want a stable order", reply.Reactions[0].Sender.Sender)
	}
}

func TestFetchMessagesStopsFetchingReactorsAtTheBudget(t *testing.T) {
	var messages []nctalk.Message
	reactions := map[int64]nctalk.ReactionList{}
	for id := int64(1); id <= backfillReactionBudget+5; id++ {
		msg := comment(id, "bob", fmt.Sprintf("message %d", id))
		msg.Reactions = map[string]int{"👍": 1}
		messages = append(messages, msg)
		reactions[id] = nctalk.ReactionList{"👍": {{ActorType: nctalk.ActorUsers, ActorID: "alice"}}}
	}
	history := &fakeHistory{messages: messages, reactions: reactions}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:  portal,
		Forward: true,
		Count:   len(messages),
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	// Every message is still backfilled; only the reaction detail is given up.
	if len(resp.Messages) != len(messages) {
		t.Errorf("got %d messages, want all %d", len(resp.Messages), len(messages))
	}
	if history.reactionCalls != backfillReactionBudget {
		t.Errorf("made %d reaction requests, want the budget of %d", history.reactionCalls, backfillReactionBudget)
	}
}

func TestFetchMessagesMarksReadHistoryTheUserHasSeen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lastRead int64
		want     bool
	}{
		{name: "already read in Talk", lastRead: 12, want: true},
		{name: "still unread in Talk", lastRead: 10, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := &fakeHistory{
				messages: []nctalk.Message{comment(11, "bob", "one"), comment(12, "bob", "two")},
				conversation: map[string]any{
					"token": backfillToken, "type": nctalk.RoomTypeGroup,
					"lastReadMessage": tc.lastRead,
				},
			}
			client, portal := newBackfillClient(t, history)

			resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
				Portal:  portal,
				Forward: true,
				Count:   10,
			})
			if err != nil {
				t.Fatalf("FetchMessages failed: %v", err)
			}
			if resp.MarkRead != tc.want {
				t.Errorf("MarkRead = %v, want %v", resp.MarkRead, tc.want)
			}
		})
	}
}

func TestFetchMessagesRejectsAnotherServersPortal(t *testing.T) {
	client, _ := newBackfillClient(t, &fakeHistory{})

	_, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:  newTestPortal("other.example.com", backfillToken),
		Forward: true,
		Count:   10,
	})
	if err == nil {
		t.Fatal("expected an error for a portal on a different Nextcloud server")
	}
}

func TestFetchMessagesIgnoresThreadBackfill(t *testing.T) {
	history := &fakeHistory{messages: []nctalk.Message{comment(10, "bob", "hello")}}
	client, portal := newBackfillClient(t, history)

	resp, err := client.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{
		Portal:     portal,
		ThreadRoot: makeMessageID(client.host(), backfillToken, 10),
		Forward:    true,
		Count:      10,
	})
	if err != nil {
		t.Fatalf("FetchMessages failed: %v", err)
	}
	// Talk's history endpoint cannot filter by thread, so there is nothing to
	// serve rather than a whole conversation pretending to be one.
	if len(resp.Messages) != 0 {
		t.Errorf("got %d messages, want none", len(resp.Messages))
	}
	if len(history.recorded()) != 0 {
		t.Error("a thread backfill should not read the conversation")
	}
}

func TestConvertMessageUsesResolvedFilePathsAsTheyAre(t *testing.T) {
	// History read as this login already resolves a file's path against their
	// own files, so re-resolving every file in a batch would be a request each
	// for an answer already in hand.
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/context"):
			t.Errorf("resolved parameters should not be looked up again, but %s was requested", r.URL.Path)
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/remote.php/dav/"):
			_, _ = w.Write([]byte("file contents"))
		default:
			writeOCS(t, w, map[string]any{})
		}
	})
	client := newTestClient(t, serverURL, "alice", Config{})
	uploader := &stubUploader{}

	msg := &talkMessage{
		Token:          backfillToken,
		MessageID:      10,
		ActorType:      nctalk.ActorUsers,
		ActorID:        "bob",
		Text:           "{file}",
		ParamsResolved: true,
		Parameters: map[string]nctalk.MessageParam{
			"file": {Type: nctalk.ParamTypeFile, ID: "200", Name: "note.txt", Path: "Talk/note.txt", MimeType: "text/plain"},
		},
	}
	converted, err := client.convertMessage(context.Background(), newTestPortal(client.host(), backfillToken), uploader, msg)
	if err != nil {
		t.Fatalf("convertMessage failed: %v", err)
	}
	if len(converted.Parts) != 1 || converted.Parts[0].Content.MsgType != event.MsgFile {
		t.Fatalf("converted %+v, want a single file part", converted.Parts)
	}
	if string(uploader.uploaded) != "file contents" {
		t.Errorf("uploaded %q, want the file fetched from the resolved path", uploader.uploaded)
	}
}
