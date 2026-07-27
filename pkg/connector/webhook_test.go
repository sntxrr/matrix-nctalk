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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

const (
	testBotSecret = "0123456789abcdef0123456789abcdef"
	testBackend   = "https://cloud.example.com/"
)

// newTestConnector builds the minimum connector the webhook handler touches:
// a logger, the bot secret, the event queue and the replay cache.
func newTestConnector(t *testing.T, queueSize int) *NCTalkConnector {
	t.Helper()
	return &NCTalkConnector{
		Bridge: &bridgev2.Bridge{Log: zerolog.Nop()},
		Config: Config{BotSecret: testBotSecret},
		queue:  make(chan *pendingEvent, queueSize),
		nonces: newNonceCache(defaultNonceRetention, defaultNonceMaxSize),
	}
}

// signBody produces the header value Talk would send for a given body.
func signBody(random, body string) string {
	mac := hmac.New(sha256.New, []byte(testBotSecret))
	mac.Write([]byte(random + body))
	return hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, nc *NCTalkConnector, body, random, signature, backend string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set(nctalk.HeaderRandom, random)
	req.Header.Set(nctalk.HeaderSignature, signature)
	req.Header.Set(nctalk.HeaderBackend, backend)

	rec := httptest.NewRecorder()
	nc.handleWebhook(rec, req)
	return rec
}

func TestWebhookAcceptsSignedEvent(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("a", 64)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, createFixtureJSON), testBackend)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	select {
	case pending := <-nc.queue:
		if pending.host != "cloud.example.com" {
			t.Errorf("host = %q, want cloud.example.com", pending.host)
		}
		if pending.evt.Type != nctalk.ActivityCreate {
			t.Errorf("activity = %q, want Create", pending.evt.Type)
		}
		if pending.receivedAt.IsZero() {
			t.Error("receivedAt was not set")
		}
	default:
		t.Fatal("event was not enqueued")
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("b", 64)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, "a different body"), testBackend)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(nc.queue) != 0 {
		t.Error("an unsigned event was enqueued")
	}
}

func TestWebhookRejectsMissingBackendHeader(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("c", 64)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, createFixtureJSON), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(nc.queue) != 0 {
		t.Error("an event with no backend was enqueued")
	}
}

// A validly signed but unparseable payload must still be acknowledged. Talk
// counts non-2xx responses against an error budget and disables the bot once it
// is exhausted, so a payload shape we do not understand must not cost us the
// whole integration.
func TestWebhookAcksUnparseablePayload(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("d", 64)
	body := `{"actor":{"id":"users/alice"}}` // valid JSON, no activity type

	rec := postWebhook(t, nc, body, random, signBody(random, body), testBackend)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so Talk does not disable the bot", rec.Code)
	}
	if len(nc.queue) != 0 {
		t.Error("an unparseable event was enqueued")
	}
}

// The same reasoning applies when the bridge cannot keep up: dropping an event
// is bad, but returning an error would eventually disable the bot entirely.
func TestWebhookAcksWhenQueueIsFull(t *testing.T) {
	nc := newTestConnector(t, 0) // unbuffered with no reader: always full
	random := strings.Repeat("e", 64)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, createFixtureJSON), testBackend)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when the queue is full", rec.Code)
	}
}

func TestWebhookRejectsOversizedBody(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("f", 64)
	huge := `{"type":"Create","padding":"` + strings.Repeat("x", maxWebhookBody+1) + `"}`

	rec := postWebhook(t, nc, huge, random, signBody(random, huge), testBackend)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(nc.queue) != 0 {
		t.Error("an oversized event was enqueued")
	}
}

func TestWebhookRejectsShortRandom(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("g", 8)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, createFixtureJSON), testBackend)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// createFixtureJSON is a single-line Create activity, kept compact so the
// signature covers exactly the bytes the test sends.
const createFixtureJSON = `{"type":"Create","actor":{"type":"Person","id":"users/alice","name":"Alice Example"},"object":{"type":"Note","id":"4711","name":"message","content":"{\"message\":\"hello\",\"parameters\":{}}","mediaType":"text/markdown"},"target":{"type":"Collection","id":"abc123token","name":"Project chat"},"published":"2026-07-26T12:00:00+00:00"}`

func TestTalkMessageFromActivity(t *testing.T) {
	evt, err := nctalk.ParseWebhookEvent([]byte(createFixtureJSON))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	received := time.Date(2026, 7, 26, 13, 0, 0, 0, time.UTC)

	msg, err := talkMessageFromActivity(evt, "abc123token", received)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if msg == nil {
		t.Fatal("expected a message")
	}
	if msg.MessageID != 4711 || msg.Token != "abc123token" {
		t.Errorf("id/token = %d/%q", msg.MessageID, msg.Token)
	}
	if msg.ActorType != "users" || msg.ActorID != "alice" {
		t.Errorf("actor = %s/%s", msg.ActorType, msg.ActorID)
	}
	if msg.Text != "hello" || !msg.IsMarkdown {
		t.Errorf("text = %q markdown = %v", msg.Text, msg.IsMarkdown)
	}
	// The published time wins over the receive time when Talk supplies it.
	if !msg.timestamp().Equal(time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("timestamp = %v, want the published time", msg.timestamp())
	}
}

func TestTalkMessageFallsBackToReceiveTime(t *testing.T) {
	body := `{"type":"Create","actor":{"type":"Person","id":"users/alice"},"object":{"type":"Note","id":"1","name":"message","content":"{\"message\":\"hi\",\"parameters\":{}}"},"target":{"type":"Collection","id":"abc123token"}}`
	evt, err := nctalk.ParseWebhookEvent([]byte(body))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	received := time.Date(2026, 7, 26, 13, 0, 0, 0, time.UTC)

	msg, err := talkMessageFromActivity(evt, "abc123token", received)
	if err != nil || msg == nil {
		t.Fatalf("build failed: %v", err)
	}
	if !msg.timestamp().Equal(received) {
		t.Errorf("timestamp = %v, want the receive time", msg.timestamp())
	}
}

// Actor types with no Matrix equivalent yield no message and no error, so they
// are skipped rather than logged as failures.
func TestTalkMessageSkipsUnbridgeableActor(t *testing.T) {
	body := `{"type":"Create","actor":{"type":"Person","id":"circles/circle1"},"object":{"type":"Note","id":"1","name":"message","content":"{\"message\":\"hi\",\"parameters\":{}}"},"target":{"type":"Collection","id":"abc123token"}}`
	evt, _ := nctalk.ParseWebhookEvent([]byte(body))

	msg, err := talkMessageFromActivity(evt, "abc123token", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg != nil {
		t.Error("an unbridgeable actor should produce no message")
	}
}

func TestTalkMessageCarriesReply(t *testing.T) {
	body := `{"type":"Create","actor":{"type":"Person","id":"users/alice"},"object":{"type":"Note","id":"4712","name":"message","content":"{\"message\":\"agreed\",\"parameters\":{}}","inReplyTo":{"type":"Note","actor":{"type":"Person","id":"users/bob"},"object":{"type":"Note","id":"4711","name":"message","content":"{\"message\":\"hi\",\"parameters\":{}}"}}},"target":{"type":"Collection","id":"abc123token"}}`
	evt, _ := nctalk.ParseWebhookEvent([]byte(body))

	msg, err := talkMessageFromActivity(evt, "abc123token", time.Now())
	if err != nil || msg == nil {
		t.Fatalf("build failed: %v", err)
	}
	if msg.ReplyToID != 4711 {
		t.Errorf("ReplyToID = %d, want 4711", msg.ReplyToID)
	}
}

// A malformed parent ID should cost only the reply relation, not the message.
func TestTalkMessageToleratesBadReplyID(t *testing.T) {
	body := `{"type":"Create","actor":{"type":"Person","id":"users/alice"},"object":{"type":"Note","id":"4712","name":"message","content":"{\"message\":\"agreed\",\"parameters\":{}}","inReplyTo":{"type":"Note","actor":{"type":"Person","id":"users/bob"},"object":{"type":"Note","id":"not-a-number","name":"message","content":"{}"}}},"target":{"type":"Collection","id":"abc123token"}}`
	evt, _ := nctalk.ParseWebhookEvent([]byte(body))

	msg, err := talkMessageFromActivity(evt, "abc123token", time.Now())
	if err != nil {
		t.Fatalf("a malformed parent ID should not fail the message: %v", err)
	}
	if msg == nil {
		t.Fatal("expected a message")
	}
	if msg.ReplyToID != 0 {
		t.Errorf("ReplyToID = %d, want 0", msg.ReplyToID)
	}
}

func TestTalkMessageRejectsMalformedActivity(t *testing.T) {
	cases := map[string]string{
		"non-numeric message ID": `{"type":"Create","actor":{"type":"Person","id":"users/alice"},"object":{"type":"Note","id":"abc","name":"message","content":"{}"},"target":{"type":"Collection","id":"t"}}`,
		"malformed actor":        `{"type":"Create","actor":{"type":"Person","id":"noslash"},"object":{"type":"Note","id":"1","name":"message","content":"{}"},"target":{"type":"Collection","id":"t"}}`,
		"bad nested content":     `{"type":"Create","actor":{"type":"Person","id":"users/alice"},"object":{"type":"Note","id":"1","name":"message","content":"not json"},"target":{"type":"Collection","id":"t"}}`,
	}
	for name, body := range cases {
		evt, err := nctalk.ParseWebhookEvent([]byte(body))
		if err != nil {
			t.Fatalf("%s: fixture failed to parse: %v", name, err)
		}
		if _, err := talkMessageFromActivity(evt, "t", time.Now()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// Talk's Like activity names the author of the message being reacted to, not
// the person reacting. Nothing in the payload identifies the reactor, so the
// activity is only good for the message ID; who reacted has to be asked for.
// This is the behaviour of a real server, confirmed against Talk 21: bob
// reacting to alice's message produces `"actor": {"id": "users/alice"}`.
func TestReactionActivityYieldsOnlyTheMessage(t *testing.T) {
	body := `{"type":"Like","actor":{"type":"Person","id":"users/alice","name":"Alice"},"object":{"type":"Note","id":"4711","name":"","content":"{\"message\":\"hi\",\"parameters\":[]}"},"target":{"type":"Collection","id":"abc123token"},"content":"👍"}`
	evt, err := nctalk.ParseWebhookEvent([]byte(body))
	if err != nil {
		t.Fatalf("fixture failed to parse: %v", err)
	}

	note, err := evt.Note()
	if err != nil {
		t.Fatalf("decode note: %v", err)
	}
	id, err := note.MessageID()
	if err != nil {
		t.Fatalf("message ID: %v", err)
	}
	if id != 4711 {
		t.Errorf("message ID = %d, want 4711", id)
	}
}

// The undone Like carries the message; the Undo wrapping it carries nothing the
// bridge can use, since neither activity names the reactor either way.
func TestUndoActivityYieldsTheNestedMessage(t *testing.T) {
	body := `{"type":"Undo","actor":{"type":"Person","id":"users/carol"},"object":{"type":"Like","actor":{"type":"Person","id":"users/alice"},"object":{"type":"Note","id":"4711","name":"message","content":"{}"},"content":"👍"},"target":{"type":"Collection","id":"abc123token"}}`
	evt, err := nctalk.ParseWebhookEvent([]byte(body))
	if err != nil {
		t.Fatalf("fixture failed to parse: %v", err)
	}

	like, err := evt.UndoneLike()
	if err != nil {
		t.Fatalf("undo decode failed: %v", err)
	}
	if like.Type != nctalk.ActivityLike {
		t.Fatalf("nested type = %q, want Like", like.Type)
	}
	note, err := like.Note()
	if err != nil {
		t.Fatalf("decode nested note: %v", err)
	}
	id, err := note.MessageID()
	if err != nil {
		t.Fatalf("message ID: %v", err)
	}
	if id != 4711 {
		t.Errorf("message ID = %d, want 4711", id)
	}
}
