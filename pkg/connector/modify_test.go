package connector

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// chatMessagePath is the endpoint an edit or delete must reach.
const chatMessagePath = nctalk.SpreedAPI + "/api/v1/chat/abc123/4711"

// readMarkerPath is the endpoint a read receipt must reach.
const readMarkerPath = nctalk.SpreedAPI + "/api/v1/chat/abc123/read"

// newTestBridgedMessage builds a stored message row for a Talk message the
// bridge sent sentAgo ago.
func newTestBridgedMessage(client *NCTalkClient, sentAgo time.Duration) *database.Message {
	return &database.Message{
		ID:        makeMessageID(client.host(), "abc123", 4711),
		Timestamp: time.Now().Add(-sentAgo),
	}
}

// newTestEdit builds the edit bridgev2 would hand to the connector.
func newTestEdit(client *NCTalkClient, body string, sentAgo time.Duration) *bridgev2.MatrixEdit {
	return &bridgev2.MatrixEdit{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{
			Event:   &event.Event{ID: "$edit1:matrix.example.com"},
			Portal:  newTestPortal(client.host(), "abc123"),
			Content: &event.MessageEventContent{MsgType: event.MsgText, Body: body},
		},
		EditTarget: newTestBridgedMessage(client, sentAgo),
	}
}

// newTestRemove builds the redaction of a bridged message.
func newTestRemove(client *NCTalkClient, sentAgo time.Duration) *bridgev2.MatrixMessageRemove {
	return &bridgev2.MatrixMessageRemove{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.RedactionEventContent]{
			Event:   &event.Event{ID: "$redact1:matrix.example.com"},
			Portal:  newTestPortal(client.host(), "abc123"),
			Content: &event.RedactionEventContent{},
		},
		TargetMessage: newTestBridgedMessage(client, sentAgo),
	}
}

func TestHandleMatrixEdit(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": 4711})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	msg := newTestEdit(client, "corrected", time.Minute)
	if err := client.HandleMatrixEdit(context.Background(), msg); err != nil {
		t.Fatalf("HandleMatrixEdit: %v", err)
	}
	if last.Method != http.MethodPut || last.Path != chatMessagePath {
		t.Errorf("request = %s %s, want PUT %s", last.Method, last.Path, chatMessagePath)
	}
	form, err := url.ParseQuery(last.Body)
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if form.Get("message") != "corrected" {
		t.Errorf("message = %q", form.Get("message"))
	}
	// Talk keeps the message ID across an edit, so only the count moves.
	if msg.EditTarget.EditCount != 1 {
		t.Errorf("EditCount = %d, want 1", msg.EditTarget.EditCount)
	}
	if want := makeMessageID(client.host(), "abc123", 4711); msg.EditTarget.ID != want {
		t.Errorf("EditTarget.ID = %q, want it unchanged", msg.EditTarget.ID)
	}
}

// Markdown reaches Talk the same way it does on a first send.
func TestHandleMatrixEditFormatting(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": 4711})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	msg := newTestEdit(client, "bold", time.Minute)
	msg.Content.Format = event.FormatHTML
	msg.Content.FormattedBody = "<strong>bold</strong>"

	if err := client.HandleMatrixEdit(context.Background(), msg); err != nil {
		t.Fatalf("HandleMatrixEdit: %v", err)
	}
	form, _ := url.ParseQuery(last.Body)
	if form.Get("message") != "**bold**" {
		t.Errorf("message = %q, want markdown", form.Get("message"))
	}
}

func TestHandleMatrixEditTooOld(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	err := client.HandleMatrixEdit(context.Background(), newTestEdit(client, "corrected", 25*time.Hour))
	if !errors.Is(err, errEditTargetTooOld) {
		t.Fatalf("err = %v, want errEditTargetTooOld", err)
	}
}

// Nothing in Talk can be changed on behalf of a Matrix user with no account
// there: the bot endpoint has no edit or delete call at all.
func TestHandleMatrixEditFromRelayedUser(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{RelayUnlinkedUsers: true})

	msg := newTestEdit(client, "corrected", time.Minute)
	msg.OrigSender = &bridgev2.OrigSender{UserID: "@bob:matrix.example.com"}

	if err := client.HandleMatrixEdit(context.Background(), msg); !errors.Is(err, errRelaySenderCannotModify) {
		t.Fatalf("err = %v, want errRelaySenderCannotModify", err)
	}
}

func TestHandleMatrixEditRejections(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"too long", http.StatusRequestEntityTooLarge, errMessageTooLong},
		{"not your message", http.StatusForbidden, errNotYourMessage},
		{"already deleted", http.StatusNotFound, errTargetMessageGone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeOCSError(w, tc.status, "nope")
			})
			client := newTestClient(t, serverURL, "alice", Config{})

			err := client.HandleMatrixEdit(context.Background(), newTestEdit(client, "corrected", time.Minute))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// Talk gives no reason for a 400, so the bridge cannot invent one, but it still
// has to fail the edit rather than report success.
func TestHandleMatrixEditRefused(t *testing.T) {
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusBadRequest, "nope")
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	msg := newTestEdit(client, "corrected", time.Minute)
	err := client.HandleMatrixEdit(context.Background(), msg)
	if err == nil || !strings.Contains(err.Error(), "would not edit") {
		t.Fatalf("err = %v, want a refusal naming the action", err)
	}
	if msg.EditTarget.EditCount != 0 {
		t.Errorf("EditCount = %d, want it left alone after a failure", msg.EditTarget.EditCount)
	}
}

func TestHandleMatrixEditEmptyBody(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	err := client.HandleMatrixEdit(context.Background(), newTestEdit(client, "   ", time.Minute))
	if !errors.Is(err, errEmptyMessage) {
		t.Fatalf("err = %v, want errEmptyMessage", err)
	}
}

func TestHandleMatrixEditTooLong(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	err := client.HandleMatrixEdit(context.Background(),
		newTestEdit(client, strings.Repeat("a", nctalk.MaxChatLength+1), time.Minute))
	if !errors.Is(err, errMessageTooLong) {
		t.Fatalf("err = %v, want errMessageTooLong", err)
	}
}

func TestHandleMatrixEditUnsupportedContent(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	msg := newTestEdit(client, "picture", time.Minute)
	msg.Content.MsgType = event.MsgImage

	if err := client.HandleMatrixEdit(context.Background(), msg); !errors.Is(err, bridgev2.ErrUnsupportedMessageType) {
		t.Fatalf("err = %v, want an unsupported type error", err)
	}
}

func TestHandleMatrixMessageRemove(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": 4712, "systemMessage": "message_deleted"})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	if err := client.HandleMatrixMessageRemove(context.Background(), newTestRemove(client, time.Minute)); err != nil {
		t.Fatalf("HandleMatrixMessageRemove: %v", err)
	}
	if last.Method != http.MethodDelete || last.Path != chatMessagePath {
		t.Errorf("request = %s %s, want DELETE %s", last.Method, last.Path, chatMessagePath)
	}
}

func TestHandleMatrixMessageRemoveTooOld(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	err := client.HandleMatrixMessageRemove(context.Background(), newTestRemove(client, 7*time.Hour))
	if !errors.Is(err, errDeleteTargetTooOld) {
		t.Fatalf("err = %v, want errDeleteTargetTooOld", err)
	}
}

func TestHandleMatrixMessageRemoveFromRelayedUser(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{RelayUnlinkedUsers: true})

	msg := newTestRemove(client, time.Minute)
	msg.OrigSender = &bridgev2.OrigSender{UserID: "@bob:matrix.example.com"}

	if err := client.HandleMatrixMessageRemove(context.Background(), msg); !errors.Is(err, errRelaySenderCannotModify) {
		t.Fatalf("err = %v, want errRelaySenderCannotModify", err)
	}
}

// A message relayed through the bot has an ID derived from its reference rather
// than a Talk message ID, so there is nothing to delete.
func TestHandleMatrixMessageRemoveRelayedMessage(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	msg := newTestRemove(client, time.Minute)
	msg.TargetMessage.ID = makeRelayedMessageID(testHost, "abc123", "deadbeef")

	if err := client.HandleMatrixMessageRemove(context.Background(), msg); !errors.Is(err, errRelayedNotModifiable) {
		t.Fatalf("err = %v, want errRelayedNotModifiable", err)
	}
}

// Talk answers 405 for anything that is not a deletable message, which reads
// differently from a message that is simply gone.
func TestHandleMatrixMessageRemoveNotDeletable(t *testing.T) {
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusMethodNotAllowed, "not a comment")
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	err := client.HandleMatrixMessageRemove(context.Background(), newTestRemove(client, time.Minute))
	if err == nil || !strings.Contains(err.Error(), "would not delete") {
		t.Fatalf("err = %v, want a refusal naming the action", err)
	}
}

// A message with no recorded send time predates the bridge knowing better, so
// Talk gets to decide rather than the bridge refusing on a guess.
func TestHandleMatrixMessageRemoveUnknownAge(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": 4712})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	msg := newTestRemove(client, 0)
	msg.TargetMessage.Timestamp = time.Time{}

	if err := client.HandleMatrixMessageRemove(context.Background(), msg); err != nil {
		t.Fatalf("HandleMatrixMessageRemove: %v", err)
	}
	if last.Path != chatMessagePath {
		t.Errorf("Path = %s, want the delete to have been attempted", last.Path)
	}
}

// newReadCapableClient returns a client whose server advertises read status.
func newReadCapableClient(t *testing.T, serverURL string) *NCTalkClient {
	t.Helper()
	client := newTestClient(t, serverURL, "alice", Config{})
	client.caps = &nctalk.Capabilities{Features: []string{nctalk.CapChatReadStatus}}
	return client
}

func TestHandleMatrixReadReceipt(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"token": "abc123"})
	})
	client := newReadCapableClient(t, serverURL)

	err := client.HandleMatrixReadReceipt(context.Background(), &bridgev2.MatrixReadReceipt{
		Portal:       newTestPortal(client.host(), "abc123"),
		ExactMessage: newTestBridgedMessage(client, time.Minute),
		ReadUpTo:     time.Now(),
	})
	if err != nil {
		t.Fatalf("HandleMatrixReadReceipt: %v", err)
	}
	if last.Path != readMarkerPath {
		t.Errorf("Path = %s, want %s", last.Path, readMarkerPath)
	}
	form, err := url.ParseQuery(last.Body)
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if form.Get("lastReadMessage") != "4711" {
		t.Errorf("lastReadMessage = %q", form.Get("lastReadMessage"))
	}
}

// Implicit receipts fire on every outgoing event and carry no message, so they
// must not cost a request each.
func TestHandleMatrixReadReceiptWithoutMessage(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a receipt with no bridged message must not reach Talk")
	})
	client := newReadCapableClient(t, serverURL)

	err := client.HandleMatrixReadReceipt(context.Background(), &bridgev2.MatrixReadReceipt{
		Portal:   newTestPortal(client.host(), "abc123"),
		Implicit: true,
		ReadUpTo: time.Now(),
	})
	if err != nil {
		t.Fatalf("HandleMatrixReadReceipt: %v", err)
	}
	if last.Path != "" {
		t.Errorf("request made to %s", last.Path)
	}
}

// Talk's marker is absolute, so a receipt that has been overtaken would drag it
// backwards and resurrect messages the user has already read.
func TestHandleMatrixReadReceiptDoesNotGoBackwards(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an outdated receipt must not move the marker")
	})
	client := newReadCapableClient(t, serverURL)

	now := time.Now()
	err := client.HandleMatrixReadReceipt(context.Background(), &bridgev2.MatrixReadReceipt{
		Portal:       newTestPortal(client.host(), "abc123"),
		ExactMessage: newTestBridgedMessage(client, time.Hour),
		ReadUpTo:     now.Add(-time.Hour),
		LastRead:     now,
	})
	if err != nil {
		t.Fatalf("HandleMatrixReadReceipt: %v", err)
	}
	if last.Path != "" {
		t.Errorf("request made to %s", last.Path)
	}
}

func TestHandleMatrixReadReceiptWithoutCapability(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a server without read status must not be asked to set a marker")
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	err := client.HandleMatrixReadReceipt(context.Background(), &bridgev2.MatrixReadReceipt{
		Portal:       newTestPortal(client.host(), "abc123"),
		ExactMessage: newTestBridgedMessage(client, time.Minute),
		ReadUpTo:     time.Now(),
	})
	if err != nil {
		t.Fatalf("HandleMatrixReadReceipt: %v", err)
	}
	if last.Path != "" {
		t.Errorf("request made to %s", last.Path)
	}
}

// A failed read receipt is not worth telling the user about, but a receipt for
// a message with no Talk ID must not be reported as delivered either.
func TestHandleMatrixReadReceiptUnusableTarget(t *testing.T) {
	serverURL, last := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a relayed message has no Talk ID to mark read")
	})
	client := newReadCapableClient(t, serverURL)

	err := client.HandleMatrixReadReceipt(context.Background(), &bridgev2.MatrixReadReceipt{
		Portal:       newTestPortal(client.host(), "abc123"),
		ExactMessage: &database.Message{ID: makeRelayedMessageID(testHost, "abc123", "deadbeef")},
		ReadUpTo:     time.Now(),
	})
	if err != nil {
		t.Fatalf("HandleMatrixReadReceipt: %v", err)
	}
	if last.Path != "" {
		t.Errorf("request made to %s", last.Path)
	}
}

// One bridge can serve several Nextcloud servers, and a message from one of
// them must never be addressed against another.
func TestTalkTargetRejectsForeignMessages(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})
	portal := newTestPortal(client.host(), "abc123")

	tests := []struct {
		name string
		id   string
	}{
		{"another server", "other.example.com" + idSep + "abc123" + idSep + "4711"},
		{"another conversation", testHost + idSep + "xyz789" + idSep + "4711"},
		{"malformed", "garbage"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := client.talkTarget(portal, &database.Message{ID: networkid.MessageID(tc.id)})
			if err == nil {
				t.Fatalf("expected an error for a %s message ID", tc.name)
			}
		})
	}
}

func TestTalkTargetRejectsMalformedPortal(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})
	portal := newTestPortal(client.host(), "abc123")
	portal.ID = "garbage"

	if _, _, err := client.talkTarget(portal, newTestBridgedMessage(client, 0)); err == nil {
		t.Fatal("expected an error for a malformed portal ID")
	}
}

func TestTalkTargetRejectsForeignPortal(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})

	_, _, err := client.talkTarget(newTestPortal("other.example.com", "abc123"), newTestBridgedMessage(client, 0))
	if err == nil || !strings.Contains(err.Error(), "different Nextcloud server") {
		t.Fatalf("err = %v, want a different-server error", err)
	}
}
