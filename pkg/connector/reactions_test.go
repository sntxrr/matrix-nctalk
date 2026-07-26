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
	"net/http"
	"net/url"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

// reactionPath is the endpoint a reaction must reach.
const reactionPath = nctalk.SpreedAPI + "/api/v1/reaction/abc123/4711"

// newTestReaction builds the reaction bridgev2 would hand to the connector,
// pointing at a message already bridged in the client's own conversation.
func newTestReaction(client *NCTalkClient, key string) *bridgev2.MatrixReaction {
	return &bridgev2.MatrixReaction{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.ReactionEventContent]{
			Event:  &event.Event{ID: "$react1:matrix.example.com"},
			Portal: newTestPortal(client.host(), "abc123"),
			Content: &event.ReactionEventContent{
				RelatesTo: event.RelatesTo{Key: key},
			},
		},
		TargetMessage: newTestTargetMessage(makeMessageID(client.host(), "abc123", 4711)),
	}
}

func TestPreHandleMatrixReaction(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	resp, err := client.PreHandleMatrixReaction(context.Background(), newTestReaction(client, "👍"))
	if err != nil {
		t.Fatalf("PreHandleMatrixReaction: %v", err)
	}
	if want := makeUserID(testHost, nctalk.ActorUsers, "alice"); resp.SenderID != want {
		t.Errorf("SenderID = %s, want %s", resp.SenderID, want)
	}
	if resp.Emoji != "👍" {
		t.Errorf("Emoji = %q", resp.Emoji)
	}
	if resp.EmojiID != makeEmojiID("👍") {
		t.Errorf("EmojiID = %q", resp.EmojiID)
	}
	// Talk imposes no cap on how many different emoji one person may use.
	if resp.MaxReactions != 0 {
		t.Errorf("MaxReactions = %d, want 0", resp.MaxReactions)
	}
}

// A qualified emoji must reach Talk byte for byte, or the webhook echo of the
// bridge's own reaction would carry a different ID and be bridged back as a
// second, duplicate reaction.
func TestPreHandleMatrixReactionDoesNotNormalise(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})
	const qualified = "❤️" // ❤️ with the variation selector

	resp, err := client.PreHandleMatrixReaction(context.Background(), newTestReaction(client, qualified))
	if err != nil {
		t.Fatalf("PreHandleMatrixReaction: %v", err)
	}
	if resp.Emoji != qualified {
		t.Errorf("Emoji = %q, want the key unchanged", resp.Emoji)
	}
	if string(resp.EmojiID) != qualified {
		t.Errorf("EmojiID = %q, want the key unchanged", resp.EmojiID)
	}
}

func TestPreHandleMatrixReactionRejectsNonEmoji(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	for name, key := range map[string]string{
		"custom image": "mxc://matrix.example.com/abcdef",
		"empty":        "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.PreHandleMatrixReaction(context.Background(), newTestReaction(client, key))
			if !errors.Is(err, errNotAnEmoji) {
				t.Errorf("err = %v, want errNotAnEmoji", err)
			}
		})
	}
}

func TestHandleMatrixReaction(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeOCS(t, w, map[string]any{})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	msg := newTestReaction(client, "👍")
	msg.Portal = newTestPortal(client.host(), "abc123")
	msg.TargetMessage = newTestTargetMessage(makeMessageID(client.host(), "abc123", 4711))
	msg.PreHandleResp = &bridgev2.MatrixReactionPreResponse{Emoji: "👍", EmojiID: makeEmojiID("👍")}

	row, err := client.HandleMatrixReaction(context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleMatrixReaction: %v", err)
	}
	if row == nil {
		t.Fatal("expected a reaction row for the bridge to fill in")
	}
	if last.Method != http.MethodPost || last.Path != reactionPath {
		t.Errorf("request = %s %s, want POST %s", last.Method, last.Path, reactionPath)
	}
	form, err := url.ParseQuery(last.Body)
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if form.Get("reaction") != "👍" {
		t.Errorf("reaction = %q", form.Get("reaction"))
	}
}

// Talk never reports the ID of a message the bridge relayed through the bot, so
// there is nothing to attach a reaction to.
func TestHandleMatrixReactionOnRelayedMessage(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	msg := newTestReaction(client, "👍")
	msg.TargetMessage = newTestTargetMessage(makeRelayedMessageID(testHost, "abc123", "deadbeef"))
	msg.PreHandleResp = &bridgev2.MatrixReactionPreResponse{Emoji: "👍"}

	_, err := client.HandleMatrixReaction(context.Background(), msg)
	if !errors.Is(err, errRelayedNotModifiable) {
		t.Fatalf("err = %v, want errRelayedNotModifiable", err)
	}
}

func TestHandleMatrixReactionRejections(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"message deleted", http.StatusNotFound, errTargetMessageGone},
		{"not a participant", http.StatusForbidden, errNotYourMessage},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeOCSError(w, tc.status, "nope")
			})
			client := newTestClient(t, serverURL, "alice", Config{})

			msg := newTestReaction(client, "👍")
			msg.Portal = newTestPortal(client.host(), "abc123")
			msg.TargetMessage = newTestTargetMessage(makeMessageID(client.host(), "abc123", 4711))
			msg.PreHandleResp = &bridgev2.MatrixReactionPreResponse{Emoji: "👍"}

			if _, err := client.HandleMatrixReaction(context.Background(), msg); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// newTestReactionRemove builds the redaction of a bridged reaction.
func newTestReactionRemove(client *NCTalkClient, emojiID networkid.EmojiID, emoji string) *bridgev2.MatrixReactionRemove {
	return &bridgev2.MatrixReactionRemove{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.RedactionEventContent]{
			Event:   &event.Event{ID: "$redact1:matrix.example.com"},
			Portal:  newTestPortal(client.host(), "abc123"),
			Content: &event.RedactionEventContent{},
		},
		TargetReaction: &database.Reaction{
			MessageID: makeMessageID(client.host(), "abc123", 4711),
			EmojiID:   emojiID,
			Emoji:     emoji,
		},
	}
}

func TestHandleMatrixReactionRemove(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	err := client.HandleMatrixReactionRemove(context.Background(),
		newTestReactionRemove(client, makeEmojiID("👍"), "👍"))
	if err != nil {
		t.Fatalf("HandleMatrixReactionRemove: %v", err)
	}
	if last.Method != http.MethodDelete || last.Path != reactionPath {
		t.Errorf("request = %s %s, want DELETE %s", last.Method, last.Path, reactionPath)
	}
}

// Rows written before the connector set an emoji ID still carry the emoji.
func TestHandleMatrixReactionRemoveFallsBackToEmoji(t *testing.T) {
	var got string
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("reaction")
		writeOCS(t, w, map[string]any{})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	err := client.HandleMatrixReactionRemove(context.Background(),
		newTestReactionRemove(client, "", "🎉"))
	if err != nil {
		t.Fatalf("HandleMatrixReactionRemove: %v", err)
	}
	if got != "🎉" {
		t.Errorf("reaction = %q, want the stored emoji", got)
	}
}

// Removing a reaction Talk no longer has reaches the state the user asked for,
// so it is not worth marking the redaction as failed.
func TestHandleMatrixReactionRemoveAlreadyGone(t *testing.T) {
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusNotFound, "no such reaction")
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	err := client.HandleMatrixReactionRemove(context.Background(),
		newTestReactionRemove(client, makeEmojiID("👍"), "👍"))
	if err != nil {
		t.Fatalf("HandleMatrixReactionRemove: %v, want it treated as done", err)
	}
}

func TestHandleMatrixReactionRemoveRejected(t *testing.T) {
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusBadRequest, "nope")
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	err := client.HandleMatrixReactionRemove(context.Background(),
		newTestReactionRemove(client, makeEmojiID("👍"), "👍"))
	if err == nil {
		t.Fatal("expected a rejection to surface")
	}
}
