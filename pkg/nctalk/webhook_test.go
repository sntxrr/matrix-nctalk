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
	"testing"
	"time"
)

// Fixtures follow the shapes built by spreed's ActivityPubHelper: the actor ID
// is type-prefixed, the note ID is a string, and the note content is a nested
// JSON *string* rather than an object.

const createFixture = `{
  "type": "Create",
  "actor": {"type": "Person", "id": "users/alice", "name": "Alice Example"},
  "object": {
    "type": "Note",
    "id": "4711",
    "name": "message",
    "content": "{\"message\":\"Hello {mention-user1}\",\"parameters\":{\"mention-user1\":{\"type\":\"user\",\"id\":\"bob\",\"name\":\"Bob Example\"}}}",
    "mediaType": "text/markdown"
  },
  "target": {"type": "Collection", "id": "abc123token", "name": "Project chat"},
  "published": "2026-07-26T12:00:00+00:00"
}`

const createWithReplyFixture = `{
  "type": "Create",
  "actor": {"type": "Person", "id": "users/alice", "name": "Alice Example"},
  "object": {
    "type": "Note",
    "id": "4712",
    "name": "message",
    "content": "{\"message\":\"agreed\",\"parameters\":{}}",
    "mediaType": "text/plain",
    "inReplyTo": {
      "type": "Note",
      "actor": {"type": "Person", "id": "users/bob", "name": "Bob Example"},
      "object": {"type": "Note", "id": "4711", "name": "message", "content": "{\"message\":\"Hello\",\"parameters\":{}}"}
    }
  },
  "target": {"type": "Collection", "id": "abc123token", "name": "Project chat"}
}`

const likeFixture = `{
  "type": "Like",
  "actor": {"type": "Person", "id": "users/alice", "name": "Alice Example"},
  "object": {"type": "Note", "id": "4711", "name": "message", "content": "{\"message\":\"Hello\",\"parameters\":{}}"},
  "target": {"type": "Collection", "id": "abc123token", "name": "Project chat"},
  "content": "👍"
}`

const undoFixture = `{
  "type": "Undo",
  "actor": {"type": "Person", "id": "users/alice", "name": "Alice Example"},
  "object": {
    "type": "Like",
    "actor": {"type": "Person", "id": "users/alice", "name": "Alice Example"},
    "object": {"type": "Note", "id": "4711", "name": "message", "content": "{\"message\":\"Hello\",\"parameters\":{}}"},
    "content": "👍"
  },
  "target": {"type": "Collection", "id": "abc123token", "name": "Project chat"}
}`

const joinFixture = `{
  "type": "Join",
  "actor": {"type": "Application", "id": "bots/bot-3f9ade12", "name": "Matrix Bridge"},
  "object": {"type": "Collection", "id": "abc123token", "name": "Project chat"}
}`

const systemMessageFixture = `{
  "type": "Activity",
  "actor": {"type": "Person", "id": "users/alice", "name": "Alice Example"},
  "object": {
    "type": "Note",
    "id": "4713",
    "name": "call_started",
    "content": "{\"message\":\"{actor} started a call\",\"parameters\":{\"actor\":{\"type\":\"user\",\"id\":\"alice\",\"name\":\"Alice Example\"}}}"
  },
  "target": {"type": "Collection", "id": "abc123token", "name": "Project chat"}
}`

func TestParseCreateActivity(t *testing.T) {
	evt, err := ParseWebhookEvent([]byte(createFixture))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if evt.Type != ActivityCreate {
		t.Fatalf("type = %q, want Create", evt.Type)
	}

	token, err := evt.Token()
	if err != nil || token != "abc123token" {
		t.Fatalf("token = %q, err = %v", token, err)
	}

	note, err := evt.Note()
	if err != nil {
		t.Fatalf("note decode failed: %v", err)
	}
	id, err := note.MessageID()
	if err != nil || id != 4711 {
		t.Fatalf("message ID = %d, err = %v", id, err)
	}
	if !note.IsMarkdown() {
		t.Error("expected markdown media type")
	}

	content, err := note.DecodeContent()
	if err != nil {
		t.Fatalf("content decode failed: %v", err)
	}
	if content.Message != "Hello {mention-user1}" {
		t.Errorf("message = %q", content.Message)
	}
	param, ok := content.Parameters["mention-user1"]
	if !ok {
		t.Fatal("mention parameter missing")
	}
	if param.Type != "user" || param.Name != "Bob Example" || param.ID != "bob" {
		t.Errorf("unexpected parameter %+v", param)
	}

	want := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	if got := evt.Timestamp(); !got.Equal(want) {
		t.Errorf("timestamp = %v, want %v", got, want)
	}
}

func TestParseCreateWithReply(t *testing.T) {
	evt, err := ParseWebhookEvent([]byte(createWithReplyFixture))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	note, err := evt.Note()
	if err != nil {
		t.Fatalf("note decode failed: %v", err)
	}
	if note.InReplyTo == nil {
		t.Fatal("expected inReplyTo to be populated")
	}
	parentID, err := note.InReplyTo.Object.MessageID()
	if err != nil || parentID != 4711 {
		t.Fatalf("parent ID = %d, err = %v", parentID, err)
	}
	if note.IsMarkdown() {
		t.Error("text/plain should not be treated as markdown")
	}
	// No published field: callers must fall back to the receive time.
	if !evt.Timestamp().IsZero() {
		t.Error("expected zero timestamp when published is absent")
	}
}

func TestParseLikeActivity(t *testing.T) {
	evt, err := ParseWebhookEvent([]byte(likeFixture))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if evt.Content != "👍" {
		t.Errorf("emoji = %q", evt.Content)
	}
	note, err := evt.Note()
	if err != nil {
		t.Fatalf("note decode failed: %v", err)
	}
	if id, _ := note.MessageID(); id != 4711 {
		t.Errorf("target message ID = %d, want 4711", id)
	}
}

func TestParseUndoActivity(t *testing.T) {
	evt, err := ParseWebhookEvent([]byte(undoFixture))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	like, err := evt.UndoneLike()
	if err != nil {
		t.Fatalf("undo object decode failed: %v", err)
	}
	if like.Type != ActivityLike {
		t.Fatalf("nested type = %q, want Like", like.Type)
	}
	if like.Content != "👍" {
		t.Errorf("nested emoji = %q", like.Content)
	}
	note, err := like.Note()
	if err != nil {
		t.Fatalf("nested note decode failed: %v", err)
	}
	if id, _ := note.MessageID(); id != 4711 {
		t.Errorf("target message ID = %d, want 4711", id)
	}
	// The token lives on the outer activity, not the nested Like.
	if token, err := evt.Token(); err != nil || token != "abc123token" {
		t.Errorf("token = %q, err = %v", token, err)
	}
}

func TestParseJoinActivityTakesTokenFromObject(t *testing.T) {
	// Join and Leave have no target field, so the token has to come from the
	// object. Reading target unconditionally would drop bot lifecycle events.
	evt, err := ParseWebhookEvent([]byte(joinFixture))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if evt.Target.ID != "" {
		t.Fatal("fixture unexpectedly has a target; the test no longer covers the fallback")
	}
	token, err := evt.Token()
	if err != nil {
		t.Fatalf("token lookup failed: %v", err)
	}
	if token != "abc123token" {
		t.Errorf("token = %q, want abc123token", token)
	}
}

func TestParseSystemMessageActivity(t *testing.T) {
	evt, err := ParseWebhookEvent([]byte(systemMessageFixture))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if evt.Type != ActivityActivity {
		t.Fatalf("type = %q, want Activity", evt.Type)
	}
	note, err := evt.Note()
	if err != nil {
		t.Fatalf("note decode failed: %v", err)
	}
	// Name carries the raw system message type, which is how a system message
	// is told apart from a chat message.
	if note.Name != "call_started" {
		t.Errorf("system message type = %q, want call_started", note.Name)
	}
}

func TestParseRejectsInvalidPayloads(t *testing.T) {
	if _, err := ParseWebhookEvent([]byte(`not json`)); err == nil {
		t.Error("expected error for non-JSON body")
	}
	if _, err := ParseWebhookEvent([]byte(`{"actor":{"id":"users/alice"}}`)); err == nil {
		t.Error("expected error for payload with no activity type")
	}
}

func TestTokenErrorsWhenAbsent(t *testing.T) {
	evt := &WebhookEvent{Type: ActivityCreate}
	if _, err := evt.Token(); err == nil {
		t.Error("expected error when no token is present")
	}
}

func TestMessageIDRejectsNonNumeric(t *testing.T) {
	note := &Note{ID: "not-a-number"}
	if _, err := note.MessageID(); err == nil {
		t.Error("expected error for non-numeric message ID")
	}
}
