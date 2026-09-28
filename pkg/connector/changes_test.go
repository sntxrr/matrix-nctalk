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
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

const changeToken = "abc123token"

// changeActivity builds the webhook Talk sends for an edit or deletion: a
// system message that names neither the changed message nor its new text.
func changeActivity(systemType, systemID string) string {
	return `{"type":"Activity","actor":{"type":"Person","id":"users/bob","name":"Bob"},` +
		`"object":{"type":"Note","id":"` + systemID + `","name":"` + systemType + `",` +
		`"content":"{\"message\":\"{actor} changed a message\",\"parameters\":{}}"},` +
		`"target":{"type":"Collection","id":"` + changeToken + `"}}`
}

// newChangeClient returns a client whose server answers a context lookup of a
// system message with that system message and the given parent, the way the
// chat API reports what an edit or deletion changed.
func newChangeClient(t *testing.T, system map[string]any) (*NCTalkClient, *recordingQueuer, *recordedRequest) {
	t.Helper()
	url, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/context") {
			writeOCS(t, w, []map[string]any{system})
			return
		}
		writeOCS(t, w, map[string]any{})
	})
	client := newTestClient(t, url, "alice", botConfig())
	rec := &recordingQueuer{}
	client.queuer = rec
	return client, rec, last
}

func TestTalkSideDeleteQueuesRemove(t *testing.T) {
	client, rec, last := newChangeClient(t, map[string]any{
		"id": 4720, "systemMessage": "message_deleted", "messageType": "system",
		"parent": map[string]any{"id": 4711, "actorType": "users", "actorId": "bob", "messageType": "comment_deleted"},
	})

	evt := mustParse(t, changeActivity(nctalk.SystemMessageDeleted, "4720"))
	if err := client.handleActivity(context.Background(), evt, changeToken, time.Now()); err != nil {
		t.Fatalf("handleActivity: %v", err)
	}
	if want := nctalk.SpreedAPI + "/api/v1/chat/" + changeToken + "/4720/context"; last.Path != want {
		t.Errorf("looked up %s, want the system message's context %s", last.Path, want)
	}
	events := rec.recorded()
	if len(events) != 1 {
		t.Fatalf("queued %d events, want 1", len(events))
	}
	remove, ok := events[0].(*simplevent.MessageRemove)
	if !ok {
		t.Fatalf("queued %T, want a message removal", events[0])
	}
	if remove.Type != bridgev2.RemoteEventMessageRemove {
		t.Errorf("event type = %v", remove.Type)
	}
	// The target is the deleted message, not the system message reporting it.
	if want := makeMessageID(client.host(), changeToken, 4711); remove.TargetMessage != want {
		t.Errorf("target = %q, want %q", remove.TargetMessage, want)
	}
	if remove.PortalKey != makePortalKey(client.host(), changeToken) {
		t.Errorf("portal key = %v", remove.PortalKey)
	}
}

func TestTalkSideEditQueuesEdit(t *testing.T) {
	client, rec, _ := newChangeClient(t, map[string]any{
		"id": 4720, "systemMessage": "message_edited", "messageType": "system",
		"parent": map[string]any{
			"id": 4711, "actorType": "users", "actorId": "bob", "actorDisplayName": "Bob",
			"messageType": "comment", "message": "corrected", "messageParameters": []any{},
			"lastEditTimestamp": 1790000000,
		},
	})

	evt := mustParse(t, changeActivity(nctalk.SystemMessageEdited, "4720"))
	if err := client.handleActivity(context.Background(), evt, changeToken, time.Now()); err != nil {
		t.Fatalf("handleActivity: %v", err)
	}
	events := rec.recorded()
	if len(events) != 1 {
		t.Fatalf("queued %d events, want 1", len(events))
	}
	edit, ok := events[0].(*simplevent.Message[*talkEdit])
	if !ok {
		t.Fatalf("queued %T, want an edit", events[0])
	}
	if edit.Type != bridgev2.RemoteEventEdit {
		t.Errorf("event type = %v", edit.Type)
	}
	if want := makeMessageID(client.host(), changeToken, 4711); edit.TargetMessage != want {
		t.Errorf("target = %q, want %q", edit.TargetMessage, want)
	}
	// Talk keeps an edited message under its author even when a moderator
	// edited it, so the edit is sent as whoever sent the original.
	if !edit.Sender.ForceEditOrigSender {
		t.Error("edit should be sent as the original message's sender")
	}
	if edit.Data.SystemMessageID != 4720 {
		t.Errorf("SystemMessageID = %d, want 4720", edit.Data.SystemMessageID)
	}

	existing := []*database.Message{{ID: edit.TargetMessage, Metadata: &MessageMetadata{}}}
	converted, err := edit.ConvertEdit(context.Background(), newTestPortal(client.host(), changeToken), nil, existing)
	if err != nil {
		t.Fatalf("ConvertEdit: %v", err)
	}
	if len(converted.ModifiedParts) != 1 {
		t.Fatalf("modified %d parts, want 1", len(converted.ModifiedParts))
	}
	part := converted.ModifiedParts[0]
	if part.Part != existing[0] {
		t.Error("the edit should replace the existing part")
	}
	if part.Content.Body != "corrected" {
		t.Errorf("body = %q, want the new text", part.Content.Body)
	}
}

// Talk reports every edit and deletion the bridge makes from Matrix back over
// the webhook, as the same system message its API answered with.
func TestMatrixEditEchoIsNotBridgedBack(t *testing.T) {
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{
			"id": 4720, "systemMessage": "message_edited",
			"parent": map[string]any{"id": 4711, "message": "corrected", "lastEditTimestamp": 1790000000},
		})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	msg := newTestEdit(client, "corrected", time.Minute)
	msg.EditTarget.Metadata = &MessageMetadata{}
	if err := client.HandleMatrixEdit(context.Background(), msg); err != nil {
		t.Fatalf("HandleMatrixEdit: %v", err)
	}
	meta := msg.EditTarget.Metadata.(*MessageMetadata)
	if meta.EditEchoID != 4720 {
		t.Fatalf("EditEchoID = %d, want the system message the edit produced", meta.EditEchoID)
	}

	portal := newTestPortal(client.host(), "abc123")
	existing := []*database.Message{msg.EditTarget}
	echo := &talkEdit{SystemMessageID: 4720, LastEditTimestamp: 1790000000, Message: &talkMessage{Text: "corrected"}}
	if _, err := client.convertTalkEdit(context.Background(), portal, nil, existing, echo); !errors.Is(err, bridgev2.ErrIgnoringRemoteEvent) {
		t.Errorf("echo converted with err = %v, want it ignored", err)
	}

	// A later edit made in Talk is a real change and must still come through.
	later := &talkEdit{SystemMessageID: 4730, LastEditTimestamp: 1790000100, Message: &talkMessage{Text: "changed in Talk"}}
	converted, err := client.convertTalkEdit(context.Background(), portal, nil, existing, later)
	if err != nil {
		t.Fatalf("later edit: %v", err)
	}
	if got := converted.ModifiedParts[0].Content.Body; got != "changed in Talk" {
		t.Errorf("body = %q", got)
	}
}

// A server whose edit response is the edited message rather than the system
// message still has its echo recognised, by the edit time.
func TestMatrixEditEchoFallsBackToEditTime(t *testing.T) {
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": 4711, "message": "corrected", "lastEditTimestamp": 1790000000})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	msg := newTestEdit(client, "corrected", time.Minute)
	msg.EditTarget.Metadata = &MessageMetadata{}
	if err := client.HandleMatrixEdit(context.Background(), msg); err != nil {
		t.Fatalf("HandleMatrixEdit: %v", err)
	}
	meta := msg.EditTarget.Metadata.(*MessageMetadata)
	if meta.EditEchoID != 0 || meta.EditEchoTS != 1790000000 {
		t.Fatalf("recorded echo = id %d ts %d, want only the edit time", meta.EditEchoID, meta.EditEchoTS)
	}

	existing := []*database.Message{msg.EditTarget}
	echo := &talkEdit{SystemMessageID: 4720, LastEditTimestamp: 1790000000, Message: &talkMessage{Text: "corrected"}}
	if !isEditEcho(existing[0], echo) {
		t.Error("an edit at the recorded time should be treated as the echo")
	}
	echo.LastEditTimestamp = 1790000100
	if isEditEcho(existing[0], echo) {
		t.Error("a later edit should not be treated as the echo")
	}
}

// Changing a file's caption would mean moving the whole file again, so the
// edit is left alone rather than re-uploaded.
func TestTalkSideCaptionEditIsIgnored(t *testing.T) {
	client := newTestClient(t, "http://127.0.0.1:1", "alice", Config{})
	edit := &talkEdit{SystemMessageID: 4720, Message: &talkMessage{
		Text: "{file} new caption",
		Parameters: nctalk.MessageParams{
			"file": {Type: nctalk.ParamTypeFile, ID: "9", Name: "x.png"},
		},
	}}
	existing := []*database.Message{{Metadata: &MessageMetadata{}}}
	_, err := client.convertTalkEdit(context.Background(), newTestPortal(client.host(), changeToken), nil, existing, edit)
	if !errors.Is(err, bridgev2.ErrIgnoringRemoteEvent) {
		t.Errorf("caption edit err = %v, want it ignored", err)
	}
}

// Without a parent the bridge cannot tell what changed, and guessing would
// redact or rewrite the wrong message.
func TestTalkSideChangeWithoutParentQueuesNothing(t *testing.T) {
	client, rec, _ := newChangeClient(t, map[string]any{"id": 4720, "systemMessage": "message_deleted"})

	evt := mustParse(t, changeActivity(nctalk.SystemMessageDeleted, "4720"))
	if err := client.handleActivity(context.Background(), evt, changeToken, time.Now()); err != nil {
		t.Fatalf("handleActivity: %v", err)
	}
	if n := len(rec.recorded()); n != 0 {
		t.Errorf("queued %d events, want 0", n)
	}
}

func TestTalkSideChangeLookupFailurePropagates(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := newTestClient(t, url, "alice", botConfig())
	rec := &recordingQueuer{}
	client.queuer = rec

	evt := mustParse(t, changeActivity(nctalk.SystemMessageEdited, "4720"))
	if err := client.handleActivity(context.Background(), evt, changeToken, time.Now()); err == nil {
		t.Fatal("expected an error when the system message cannot be read")
	}
	if n := len(rec.recorded()); n != 0 {
		t.Errorf("queued %d events, want 0", n)
	}
}
