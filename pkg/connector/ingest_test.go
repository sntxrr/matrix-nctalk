package connector

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// recordingQueuer captures the events the connector hands to the bridge.
type recordingQueuer struct {
	events []bridgev2.RemoteEvent
}

func (r *recordingQueuer) QueueRemoteEvent(_ *bridgev2.UserLogin, evt bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
	r.events = append(r.events, evt)
	return bridgev2.EventHandlingResultSuccess
}

// newIngestClient returns a client whose remote events are recorded.
func newIngestClient(t *testing.T) (*NCTalkClient, *recordingQueuer) {
	t.Helper()
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"token": "abc123token", "type": nctalk.RoomTypeGroup})
	})
	client := newTestClient(t, url, "alice", botConfig())
	rec := &recordingQueuer{}
	client.queuer = rec
	return client, rec
}

func mustParse(t *testing.T, body string) *nctalk.WebhookEvent {
	t.Helper()
	evt, err := nctalk.ParseWebhookEvent([]byte(body))
	if err != nil {
		t.Fatalf("fixture failed to parse: %v", err)
	}
	return evt
}

func TestHandleCreateQueuesMessage(t *testing.T) {
	client, rec := newIngestClient(t)

	err := client.handleCreate(context.Background(), mustParse(t, createFixtureJSON), "abc123token", time.Now())
	if err != nil {
		t.Fatalf("handleCreate failed: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("queued %d events, want 1", len(rec.events))
	}

	msg, ok := rec.events[0].(*simplevent.Message[*talkMessage])
	if !ok {
		t.Fatalf("queued %T, want a message event", rec.events[0])
	}
	if msg.Type != bridgev2.RemoteEventMessage {
		t.Errorf("event type = %v", msg.Type)
	}
	if !msg.CreatePortal {
		t.Error("CreatePortal should be set so an unseen conversation gets a room")
	}
	if msg.ID != makeMessageID(client.host(), "abc123token", 4711) {
		t.Errorf("message ID = %q", msg.ID)
	}
	if msg.PortalKey != makePortalKey(client.host(), "abc123token") {
		t.Errorf("portal key = %v", msg.PortalKey)
	}
	// Talk delivers webhooks asynchronously, so ordering relies on the message
	// ID being carried as the stream order rather than on arrival order.
	if msg.StreamOrder != 4711 {
		t.Errorf("StreamOrder = %d, want the Talk message ID", msg.StreamOrder)
	}
	if msg.Sender.Sender != makeUserID(client.host(), nctalk.ActorUsers, "alice") {
		t.Errorf("sender = %q", msg.Sender.Sender)
	}
	if !msg.Sender.IsFromMe {
		t.Error("a message from the logged-in user should be marked as from me")
	}
	if msg.ConvertMessageFunc == nil {
		t.Error("no conversion function was attached")
	}
}

func TestHandleCreateSkipsUnbridgeableActor(t *testing.T) {
	client, rec := newIngestClient(t)
	body := `{"type":"Create","actor":{"type":"Person","id":"circles/c1"},"object":{"type":"Note","id":"1","name":"message","content":"{\"message\":\"hi\",\"parameters\":{}}"},"target":{"type":"Collection","id":"abc123token"}}`

	if err := client.handleCreate(context.Background(), mustParse(t, body), "abc123token", time.Now()); err != nil {
		t.Fatalf("handleCreate failed: %v", err)
	}
	if len(rec.events) != 0 {
		t.Errorf("queued %d events, want 0", len(rec.events))
	}
}

func TestHandleCreatePropagatesError(t *testing.T) {
	client, _ := newIngestClient(t)
	body := `{"type":"Create","actor":{"type":"Person","id":"users/alice"},"object":{"type":"Note","id":"not-a-number","name":"message","content":"{}"},"target":{"type":"Collection","id":"abc123token"}}`

	if err := client.handleCreate(context.Background(), mustParse(t, body), "abc123token", time.Now()); err == nil {
		t.Fatal("expected an error for a malformed message ID")
	}
}

func TestHandleReactionQueuesReaction(t *testing.T) {
	client, rec := newIngestClient(t)
	body := `{"type":"Like","actor":{"type":"Person","id":"users/bob"},"object":{"type":"Note","id":"4711","name":"message","content":"{}"},"target":{"type":"Collection","id":"abc123token"},"content":"👍"}`

	if err := client.handleReaction(context.Background(), mustParse(t, body), "abc123token", time.Now()); err != nil {
		t.Fatalf("handleReaction failed: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("queued %d events, want 1", len(rec.events))
	}

	reaction, ok := rec.events[0].(*simplevent.Reaction)
	if !ok {
		t.Fatalf("queued %T, want a reaction event", rec.events[0])
	}
	if reaction.Type != bridgev2.RemoteEventReaction {
		t.Errorf("event type = %v, want reaction", reaction.Type)
	}
	if reaction.Emoji != "👍" || reaction.EmojiID != makeEmojiID("👍") {
		t.Errorf("emoji = %q id = %q", reaction.Emoji, reaction.EmojiID)
	}
	if reaction.TargetMessage != makeMessageID(client.host(), "abc123token", 4711) {
		t.Errorf("target = %q", reaction.TargetMessage)
	}
	if reaction.Sender.Sender != makeUserID(client.host(), nctalk.ActorUsers, "bob") {
		t.Errorf("sender = %q", reaction.Sender.Sender)
	}
}

func TestHandleUndoQueuesReactionRemoval(t *testing.T) {
	client, rec := newIngestClient(t)
	body := `{"type":"Undo","actor":{"type":"Person","id":"users/bob"},"object":{"type":"Like","actor":{"type":"Person","id":"users/bob"},"object":{"type":"Note","id":"4711","name":"message","content":"{}"},"content":"👍"},"target":{"type":"Collection","id":"abc123token"}}`

	if err := client.handleUndo(context.Background(), mustParse(t, body), "abc123token", time.Now()); err != nil {
		t.Fatalf("handleUndo failed: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("queued %d events, want 1", len(rec.events))
	}

	reaction := rec.events[0].(*simplevent.Reaction)
	if reaction.Type != bridgev2.RemoteEventReactionRemove {
		t.Errorf("event type = %v, want reaction removal", reaction.Type)
	}
	if reaction.Emoji != "👍" {
		t.Errorf("emoji = %q", reaction.Emoji)
	}
}

// Talk wraps other activities in Undo too; only reaction undos are meaningful.
func TestHandleUndoIgnoresNonReaction(t *testing.T) {
	client, rec := newIngestClient(t)
	body := `{"type":"Undo","actor":{"type":"Person","id":"users/bob"},"object":{"type":"Create","actor":{"type":"Person","id":"users/bob"},"object":{"type":"Note","id":"4711","name":"message","content":"{}"}},"target":{"type":"Collection","id":"abc123token"}}`

	if err := client.handleUndo(context.Background(), mustParse(t, body), "abc123token", time.Now()); err != nil {
		t.Fatalf("handleUndo failed: %v", err)
	}
	if len(rec.events) != 0 {
		t.Errorf("queued %d events, want 0", len(rec.events))
	}
}

func TestHandleBotJoinQueuesResync(t *testing.T) {
	client, rec := newIngestClient(t)

	if err := client.handleBotJoin(context.Background(), "abc123token"); err != nil {
		t.Fatalf("handleBotJoin failed: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("queued %d events, want 1", len(rec.events))
	}

	resync, ok := rec.events[0].(*simplevent.ChatResync)
	if !ok {
		t.Fatalf("queued %T, want a chat resync", rec.events[0])
	}
	if !resync.CreatePortal {
		t.Error("enabling the bot in a new conversation should create its portal")
	}
	if resync.PortalKey != makePortalKey(client.host(), "abc123token") {
		t.Errorf("portal key = %v", resync.PortalKey)
	}
}

// Membership may have changed while the bot was absent, so cached routing for
// the conversation must be dropped on both join and leave.
func TestBotLifecycleInvalidatesRouting(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(c *NCTalkClient) error
	}{
		{"join", func(c *NCTalkClient) error { return c.handleBotJoin(context.Background(), "abc123token") }},
		{"leave", func(c *NCTalkClient) error { return c.handleBotLeave(context.Background(), "abc123token") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newIngestClient(t)
			router := client.Main.router
			router.primary[routeKey(client.host(), "abc123token")] = client.UserLogin

			if err := tc.run(client); err != nil {
				t.Fatalf("handler failed: %v", err)
			}

			router.mu.RLock()
			_, stillCached := router.primary[routeKey(client.host(), "abc123token")]
			router.mu.RUnlock()
			if stillCached {
				t.Error("routing should be invalidated")
			}
		})
	}
}

func TestHandleBotLeaveQueuesNothing(t *testing.T) {
	client, rec := newIngestClient(t)

	if err := client.handleBotLeave(context.Background(), "abc123token"); err != nil {
		t.Fatalf("handleBotLeave failed: %v", err)
	}
	// The portal is kept so history is not lost.
	if len(rec.events) != 0 {
		t.Errorf("queued %d events, want 0", len(rec.events))
	}
}

func TestProcessEventIgnoresUnbridgedConversation(t *testing.T) {
	nc := &NCTalkConnector{
		Bridge: newQuietBridge(),
		Config: botConfig(),
	}
	nc.router = newLoginRouter(&fakeLogins{})

	pending := &pendingEvent{
		evt:        mustParse(t, createFixtureJSON),
		host:       "cloud.example.com",
		receivedAt: time.Now(),
	}
	if err := nc.processEvent(context.Background(), pending); err != nil {
		t.Fatalf("an unbridged conversation should be ignored, got %v", err)
	}
}

func TestProcessEventRejectsActivityWithNoToken(t *testing.T) {
	nc := &NCTalkConnector{Bridge: newQuietBridge()}
	nc.router = newLoginRouter(&fakeLogins{})

	pending := &pendingEvent{
		evt:        &nctalk.WebhookEvent{Type: nctalk.ActivityCreate},
		host:       "cloud.example.com",
		receivedAt: time.Now(),
	}
	if err := nc.processEvent(context.Background(), pending); err == nil {
		t.Fatal("expected an error when the activity carries no token")
	}
}

func TestProcessEventRoutesByActivityType(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"token": "abc123token", "type": nctalk.RoomTypeGroup})
	})
	client := newTestClient(t, url, "alice", botConfig())
	rec := &recordingQueuer{}
	client.queuer = rec

	nc := client.Main
	nc.router = newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})
	host := client.host()

	like := `{"type":"Like","actor":{"type":"Person","id":"users/bob"},"object":{"type":"Note","id":"4711","name":"message","content":"{}"},"target":{"type":"Collection","id":"abc123token"},"content":"👍"}`
	join := `{"type":"Join","actor":{"type":"Application","id":"bots/bot-1","name":"Matrix Bridge"},"object":{"type":"Collection","id":"abc123token"}}`
	system := `{"type":"Activity","actor":{"type":"Person","id":"users/alice"},"object":{"type":"Note","id":"5","name":"call_started","content":"{\"message\":\"{actor} started a call\",\"parameters\":{}}"},"target":{"type":"Collection","id":"abc123token"}}`
	unknown := `{"type":"Announce","actor":{"type":"Person","id":"users/alice"},"target":{"type":"Collection","id":"abc123token"}}`

	tests := []struct {
		name       string
		body       string
		wantEvents int
	}{
		{"create", createFixtureJSON, 1},
		{"like", like, 1},
		{"join", join, 1},
		// System messages are recognised but not yet mapped to Matrix.
		{"system message", system, 0},
		{"unknown activity", unknown, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec.events = nil
			pending := &pendingEvent{evt: mustParse(t, tc.body), host: host, receivedAt: time.Now()}
			if err := nc.processEvent(context.Background(), pending); err != nil {
				t.Fatalf("processEvent failed: %v", err)
			}
			if len(rec.events) != tc.wantEvents {
				t.Errorf("queued %d events, want %d", len(rec.events), tc.wantEvents)
			}
		})
	}
}

// The bridge cannot receive webhooks without an HTTP server, and discovering
// that at startup is far better than silently dropping every event.
func TestRegisterWebhookRequiresHTTPServer(t *testing.T) {
	nc := &NCTalkConnector{Bridge: newQuietBridge()}
	if err := nc.registerWebhook(context.Background()); err == nil {
		t.Fatal("expected an error when the Matrix connector exposes no HTTP server")
	}
}

// The worker loop must drain the queue and survive handler errors, or one bad
// event would stall ingest for everyone.
func TestProcessEventsDrainsQueue(t *testing.T) {
	nc := &NCTalkConnector{Bridge: newQuietBridge(), Config: botConfig()}
	nc.router = newLoginRouter(&fakeLogins{})
	nc.queue = make(chan *pendingEvent, 4)

	done := make(chan struct{})
	go func() {
		nc.processEvents()
		close(done)
	}()

	// One event that errors (no token) and one that is simply unbridged.
	nc.queue <- &pendingEvent{evt: &nctalk.WebhookEvent{Type: nctalk.ActivityCreate}, host: "cloud.example.com", receivedAt: time.Now()}
	nc.queue <- &pendingEvent{evt: mustParse(t, createFixtureJSON), host: "cloud.example.com", receivedAt: time.Now()}

	close(nc.queue)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("processEvents did not drain the queue and exit")
	}
}

// The log context closures are only invoked when something is actually logged,
// so exercise them explicitly to be sure they cannot panic in production.
func TestEventLogContextsAreSafe(t *testing.T) {
	client, rec := newIngestClient(t)

	if err := client.handleCreate(context.Background(), mustParse(t, createFixtureJSON), "abc123token", time.Now()); err != nil {
		t.Fatalf("handleCreate failed: %v", err)
	}
	msg := rec.events[0].(*simplevent.Message[*talkMessage])
	if msg.LogContext == nil {
		t.Fatal("no log context on the message event")
	}
	msg.LogContext(zerolog.Nop().With())

	rec.events = nil
	like := `{"type":"Like","actor":{"type":"Person","id":"users/bob"},"object":{"type":"Note","id":"4711","name":"message","content":"{}"},"target":{"type":"Collection","id":"abc123token"},"content":"👍"}`
	if err := client.handleReaction(context.Background(), mustParse(t, like), "abc123token", time.Now()); err != nil {
		t.Fatalf("handleReaction failed: %v", err)
	}
	reaction := rec.events[0].(*simplevent.Reaction)
	if reaction.LogContext == nil {
		t.Fatal("no log context on the reaction event")
	}
	reaction.LogContext(zerolog.Nop().With())
}
