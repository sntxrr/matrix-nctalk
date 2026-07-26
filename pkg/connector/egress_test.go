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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// chatPath is the endpoint a message send must reach.
const chatPath = nctalk.SpreedAPI + "/api/v1/chat/abc123"

// botMessagePath is the endpoint a relayed message must reach instead.
const botMessagePath = nctalk.SpreedAPI + "/api/v1/bot/abc123/message"

// sendRecorder is a Nextcloud stand-in that keeps every request body, which is
// what the retry paths need: the interesting request is not the last one.
type sendRecorder struct {
	url string

	mu   sync.Mutex
	sent []url.Values
}

// bodies returns the decoded form bodies of the requests received so far.
func (s *sendRecorder) bodies() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.sent...)
}

// recordSends starts a server that records each request and answers it with
// respond, which is given the one-based request number.
func recordSends(t *testing.T, respond func(n int, w http.ResponseWriter)) *sendRecorder {
	t.Helper()
	rec := &sendRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(raw))
		if err != nil {
			t.Errorf("parse request body %q: %v", raw, err)
		}
		rec.mu.Lock()
		rec.sent = append(rec.sent, form)
		n := len(rec.sent)
		rec.mu.Unlock()
		respond(n, w)
	}))
	t.Cleanup(srv.Close)
	rec.url = srv.URL
	return rec
}

// newTextMessage builds a plain outgoing text message for the given client.
func newTextMessage(client *NCTalkClient, body string) *bridgev2.MatrixMessage {
	return newTestMatrixMessage(
		newTestPortal(client.host(), "abc123"),
		&event.MessageEventContent{MsgType: event.MsgText, Body: body},
	)
}

func TestHandleMatrixMessage(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{
			"id":        4711,
			"token":     "abc123",
			"actorType": nctalk.ActorUsers,
			"actorId":   "alice",
			"timestamp": 1700000000,
		})
	})
	client := newTestClient(t, serverURL, "alice", Config{})
	msg := newTextMessage(client, "hello")

	resp, err := client.HandleMatrixMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleMatrixMessage: %v", err)
	}

	if last.Path != chatPath {
		t.Errorf("Path = %s, want %s", last.Path, chatPath)
	}
	if last.Method != http.MethodPost {
		t.Errorf("Method = %s, want POST", last.Method)
	}
	form, err := url.ParseQuery(last.Body)
	if err != nil {
		t.Fatalf("parse sent body: %v", err)
	}
	if form.Get("message") != "hello" {
		t.Errorf("message = %q", form.Get("message"))
	}

	// The Talk message ID is what makes the webhook echo of this message
	// recognisable as already bridged.
	wantID := makeMessageID(client.host(), "abc123", 4711)
	if resp.DB.ID != wantID {
		t.Errorf("DB.ID = %q, want %q", resp.DB.ID, wantID)
	}
	if resp.StreamOrder != 4711 {
		t.Errorf("StreamOrder = %d, want 4711", resp.StreamOrder)
	}
	if want := makeUserID(client.host(), nctalk.ActorUsers, "alice"); resp.DB.SenderID != want {
		t.Errorf("DB.SenderID = %q, want %q", resp.DB.SenderID, want)
	}
	// Talk measures its edit and delete windows from its own timestamp.
	if want := time.Unix(1700000000, 0); !resp.DB.Timestamp.Equal(want) {
		t.Errorf("DB.Timestamp = %v, want %v", resp.DB.Timestamp, want)
	}
	meta, ok := resp.DB.Metadata.(*MessageMetadata)
	if !ok {
		t.Fatalf("DB.Metadata = %T, want *MessageMetadata", resp.DB.Metadata)
	}
	if meta.ReferenceID != referenceID(msg.Event.ID) {
		t.Errorf("ReferenceID = %q", meta.ReferenceID)
	}
	if meta.SentViaBot {
		t.Error("a message sent as the user should not be marked as sent via the bot")
	}
}

func TestHandleMatrixMessageReply(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": 4712, "timestamp": 1700000001})
	})
	client := newTestClient(t, serverURL, "alice", Config{})
	msg := newTextMessage(client, "answering")
	msg.ReplyTo = newTestTargetMessage(makeMessageID(client.host(), "abc123", 42))

	if _, err := client.HandleMatrixMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMatrixMessage: %v", err)
	}
	form, _ := url.ParseQuery(last.Body)
	if form.Get("replyTo") != "42" {
		t.Errorf("replyTo = %q, want 42", form.Get("replyTo"))
	}
}

// Talk answers 400 when it will not accept the parent of a reply. Losing the
// reply relation is much better than losing a message the user already sent, so
// the send is retried without it.
func TestHandleMatrixMessageRetriesWithoutRejectedRelation(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*NCTalkClient, *bridgev2.MatrixMessage)
		relates string
	}{
		{
			name:    "reply",
			relates: "replyTo",
			setup: func(c *NCTalkClient, m *bridgev2.MatrixMessage) {
				m.ReplyTo = newTestTargetMessage(makeMessageID(c.host(), "abc123", 42))
			},
		},
		{
			name:    "thread root",
			relates: "threadId",
			setup: func(c *NCTalkClient, m *bridgev2.MatrixMessage) {
				m.ThreadRoot = newTestTargetMessage(makeMessageID(c.host(), "abc123", 7))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sent := recordSends(t, func(n int, w http.ResponseWriter) {
				if n == 1 {
					writeOCSError(w, http.StatusBadRequest, "invalid "+tc.relates)
					return
				}
				writeOCS(t, w, map[string]any{"id": 4713, "timestamp": 1700000002})
			})

			client := newTestClient(t, sent.url, "alice", Config{})
			msg := newTextMessage(client, "answering")
			tc.setup(client, msg)

			resp, err := client.HandleMatrixMessage(context.Background(), msg)
			if err != nil {
				t.Fatalf("HandleMatrixMessage: %v", err)
			}
			if resp.StreamOrder != 4713 {
				t.Errorf("StreamOrder = %d, want 4713", resp.StreamOrder)
			}

			bodies := sent.bodies()
			if len(bodies) != 2 {
				t.Fatalf("sent %d requests, want 2", len(bodies))
			}
			if bodies[0].Get(tc.relates) == "" {
				t.Errorf("first request had no %s", tc.relates)
			}
			// The point of the retry is that the relation is gone; keeping it would
			// just be rejected again.
			if bodies[1].Get(tc.relates) != "" {
				t.Errorf("retry still carried %s = %q", tc.relates, bodies[1].Get(tc.relates))
			}
			if bodies[1].Get("message") != "answering" {
				t.Errorf("retry message = %q", bodies[1].Get("message"))
			}
			// The reference identifies the message, so the retry has to reuse it
			// rather than looking like a second message.
			if bodies[1].Get("referenceId") != bodies[0].Get("referenceId") {
				t.Error("retry used a different referenceId")
			}
		})
	}
}

// A relation Talk keeps rejecting means the send genuinely failed, and the
// original rejection is what the user should see.
func TestHandleMatrixMessageReplyRejectedTwice(t *testing.T) {
	var calls atomic.Int32
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeOCSError(w, http.StatusBadRequest, "invalid replyTo")
	})
	client := newTestClient(t, serverURL, "alice", Config{})
	msg := newTextMessage(client, "answering")
	msg.ReplyTo = newTestTargetMessage(makeMessageID(client.host(), "abc123", 42))

	_, err := client.HandleMatrixMessage(context.Background(), msg)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !nctalk.IsBadRequest(err) {
		t.Errorf("err = %v, want a bad request error", err)
	}
	if calls.Load() != 2 {
		t.Errorf("server calls = %d, want 2", calls.Load())
	}
}

// A message with no relation to drop must not be retried; a second identical
// send would post the message twice if Talk had in fact accepted the first.
func TestHandleMatrixMessageDoesNotRetryWithoutRelation(t *testing.T) {
	var calls atomic.Int32
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeOCSError(w, http.StatusBadRequest, "message empty")
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	if _, err := client.HandleMatrixMessage(context.Background(), newTextMessage(client, "hello")); err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Errorf("server calls = %d, want 1", calls.Load())
	}
}

func TestHandleMatrixMessageServerRejections(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"too long", http.StatusRequestEntityTooLarge, errMessageTooLong},
		{"read only conversation", http.StatusForbidden, errNotAllowedInConversation},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeOCSError(w, tc.status, "nope")
			})
			client := newTestClient(t, serverURL, "alice", Config{})

			_, err := client.HandleMatrixMessage(context.Background(), newTextMessage(client, "hello"))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A revoked app password fails every later send too, so the login is flagged
// rather than left looking connected.
func TestHandleMatrixMessageUnauthorized(t *testing.T) {
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	_, err := client.HandleMatrixMessage(context.Background(), newTextMessage(client, "hello"))
	if !nctalk.IsUnauthorized(err) {
		t.Fatalf("err = %v, want an unauthorized error", err)
	}
}

// One bridge can serve several Nextcloud servers, so a login must refuse a
// portal that is not on its own.
func TestHandleMatrixMessageWrongServer(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})
	msg := newTestMatrixMessage(
		newTestPortal("other.example.com", "abc123"),
		&event.MessageEventContent{MsgType: event.MsgText, Body: "hello"},
	)

	_, err := client.HandleMatrixMessage(context.Background(), msg)
	if err == nil || !strings.Contains(err.Error(), "different Nextcloud server") {
		t.Fatalf("err = %v, want a different-server error", err)
	}
}

func TestHandleMatrixMessageMalformedPortal(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})
	msg := newTestMatrixMessage(
		&bridgev2.Portal{Portal: newTestPortal(testHost, "abc123").Portal},
		&event.MessageEventContent{MsgType: event.MsgText, Body: "hello"},
	)
	msg.Portal.ID = "garbage"

	if _, err := client.HandleMatrixMessage(context.Background(), msg); err == nil {
		t.Fatal("expected an error for a malformed portal ID")
	}
}

// A Matrix user with no Nextcloud account is relayed through the bot, which is
// the only way to post without impersonating someone.
func TestHandleMatrixMessageRelay(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	client := newTestClient(t, serverURL, "alice", Config{
		BotSecret:          strings.Repeat("s", 32),
		RelayUnlinkedUsers: true,
	})

	msg := newTextMessage(client, "**Bob**: hello")
	msg.OrigSender = &bridgev2.OrigSender{UserID: "@bob:matrix.example.com"}

	resp, err := client.HandleMatrixMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleMatrixMessage: %v", err)
	}

	if last.Path != botMessagePath {
		t.Errorf("Path = %s, want %s", last.Path, botMessagePath)
	}
	form, _ := url.ParseQuery(last.Body)
	if form.Get("message") != "**Bob**: hello" {
		t.Errorf("message = %q", form.Get("message"))
	}

	// The bot endpoint acknowledges without reporting the ID Talk assigned, so
	// the row is keyed on the reference instead.
	wantID := makeRelayedMessageID(client.host(), "abc123", referenceID(msg.Event.ID))
	if resp.DB.ID != wantID {
		t.Errorf("DB.ID = %q, want %q", resp.DB.ID, wantID)
	}
	if resp.StreamOrder != 0 {
		t.Errorf("StreamOrder = %d, want 0 for a message with no known Talk ID", resp.StreamOrder)
	}
	meta := resp.DB.Metadata.(*MessageMetadata)
	if !meta.SentViaBot {
		t.Error("a relayed message should be marked as sent via the bot")
	}
}

// Relaying posts as the bridge rather than as a person, so it stays off until an
// administrator turns it on.
func TestHandleMatrixMessageRelayDisabled(t *testing.T) {
	var calls atomic.Int32
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusCreated)
	})
	client := newTestClient(t, serverURL, "alice", Config{BotSecret: strings.Repeat("s", 32)})

	msg := newTextMessage(client, "**Bob**: hello")
	msg.OrigSender = &bridgev2.OrigSender{UserID: "@bob:matrix.example.com"}

	_, err := client.HandleMatrixMessage(context.Background(), msg)
	if !errors.Is(err, errRelayDisabled) {
		t.Fatalf("err = %v, want errRelayDisabled", err)
	}
	if calls.Load() != 0 {
		t.Errorf("server calls = %d, want 0", calls.Load())
	}
}

func TestHandleMatrixMessageRelayRejected(t *testing.T) {
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	})
	client := newTestClient(t, serverURL, "alice", Config{
		BotSecret:          strings.Repeat("s", 32),
		RelayUnlinkedUsers: true,
	})

	msg := newTextMessage(client, "**Bob**: hello")
	msg.OrigSender = &bridgev2.OrigSender{UserID: "@bob:matrix.example.com"}

	_, err := client.HandleMatrixMessage(context.Background(), msg)
	if !errors.Is(err, errMessageTooLong) {
		t.Fatalf("err = %v, want errMessageTooLong", err)
	}
}

// Conversion failures must be reported before anything is sent.
func TestHandleMatrixMessageUnsupportedContent(t *testing.T) {
	var calls atomic.Int32
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	// Files take the WebDAV route instead; a location has nowhere to go at all.
	msg := newTestMatrixMessage(
		newTestPortal(client.host(), "abc123"),
		&event.MessageEventContent{MsgType: event.MsgLocation, Body: "somewhere"},
	)
	_, err := client.HandleMatrixMessage(context.Background(), msg)
	if !errors.Is(err, bridgev2.ErrUnsupportedMessageType) {
		t.Fatalf("err = %v, want ErrUnsupportedMessageType", err)
	}
	if calls.Load() != 0 {
		t.Errorf("server calls = %d, want 0", calls.Load())
	}
}
