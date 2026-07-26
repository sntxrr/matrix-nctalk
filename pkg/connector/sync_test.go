package connector

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/id"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// fakePortals stands in for the bridge's portal store.
type fakePortals struct {
	bridged map[networkid.PortalID]id.RoomID
	err     error
}

func (f *fakePortals) GetExistingPortalByKey(_ context.Context, key networkid.PortalKey) (*bridgev2.Portal, error) {
	if f.err != nil {
		return nil, f.err
	}
	mxid, ok := f.bridged[key.ID]
	if !ok {
		return nil, nil
	}
	return &bridgev2.Portal{Portal: &database.Portal{PortalKey: key, MXID: mxid, Metadata: &PortalMetadata{}}}, nil
}

// newSyncClient wires a client whose conversation list and portal store are
// both under the test's control. bridgedTokens are the conversations that
// already have a Matrix room.
//
// The returned counter records how many times the conversation list was read,
// which is how a test sees that a sync pass ran.
func newSyncClient(t *testing.T, convs []map[string]any, bridgedTokens ...string) (*NCTalkClient, *recordingQueuer, *atomic.Int64) {
	t.Helper()

	var listCalls atomic.Int64
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v4/room") {
			listCalls.Add(1)
			writeOCS(t, w, convs)
			return
		}
		// Anything else is the router probing participation, which succeeds.
		writeOCS(t, w, map[string]any{"token": "one", "type": nctalk.RoomTypeGroup})
	})

	client := newTestClient(t, url, "alice", botConfig())
	rec := &recordingQueuer{}
	client.queuer = rec

	bridged := make(map[networkid.PortalID]id.RoomID, len(bridgedTokens))
	for _, token := range bridgedTokens {
		bridged[makePortalID(client.host(), token)] = id.RoomID("!" + token + ":matrix.example.com")
	}
	client.portalFinder = &fakePortals{bridged: bridged}
	client.Main.router = newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})
	return client, rec, &listCalls
}

func TestSyncConversationsResyncsBridgedRooms(t *testing.T) {
	convs := []map[string]any{
		{"token": "bridged1", "type": nctalk.RoomTypeGroup, "displayName": "One", "lastActivity": 1700000500},
		{"token": "notbridged", "type": nctalk.RoomTypeGroup, "displayName": "Two", "lastActivity": 1700000600},
	}
	client, rec, _ := newSyncClient(t, convs, "bridged1")

	client.syncConversations(context.Background())

	if len(rec.events) != 1 {
		t.Fatalf("queued %d events, want only the bridged conversation", len(rec.events))
	}
	resync, ok := rec.events[0].(*simplevent.ChatResync)
	if !ok {
		t.Fatalf("queued %T, want a chat resync", rec.events[0])
	}
	if resync.PortalKey != makePortalKey(client.host(), "bridged1") {
		t.Errorf("portal key = %v", resync.PortalKey)
	}
	if resync.CreatePortal {
		// A timer is not a reason to pull every conversation on the server into
		// Matrix.
		t.Error("a periodic resync must not create portals")
	}
	// The last activity time is what tells bridgev2 whether Talk holds anything
	// newer than the last bridged message.
	if !resync.LatestMessageTS.Equal(time.Unix(1700000500, 0)) {
		t.Errorf("LatestMessageTS = %v, want the conversation's last activity", resync.LatestMessageTS)
	}
	if resync.GetChatInfoFunc == nil {
		t.Error("the resync should refresh the room's name, topic and members")
	}
}

func TestSyncConversationsSkipsUnbridgeableConversations(t *testing.T) {
	convs := []map[string]any{
		// The changelog conversation is bridge noise, and a former one-to-one is
		// one Talk itself refuses to put a bot in.
		{"token": "changelog", "type": nctalk.RoomTypeChangelog, "lastActivity": 1700000500},
		{"token": "former", "type": nctalk.RoomTypeOneToOneFormer, "lastActivity": 1700000500},
	}
	client, rec, _ := newSyncClient(t, convs, "changelog", "former")

	client.syncConversations(context.Background())

	if len(rec.events) != 0 {
		t.Errorf("queued %d events, want none", len(rec.events))
	}
}

func TestSyncConversationsLeavesRoomsWithoutAPortalAlone(t *testing.T) {
	convs := []map[string]any{{"token": "one", "type": nctalk.RoomTypeGroup, "lastActivity": 1700000500}}
	client, rec, _ := newSyncClient(t, convs)

	client.syncConversations(context.Background())

	if len(rec.events) != 0 {
		t.Errorf("queued %d events for conversations nobody has bridged", len(rec.events))
	}
}

func TestSyncConversationsSurvivesAFailedList(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusInternalServerError, "the server is having a moment")
	})
	client := newTestClient(t, url, "alice", botConfig())
	rec := &recordingQueuer{}
	client.queuer = rec
	client.portalFinder = &fakePortals{}

	// A failed pass is logged and the loop carries on to the next interval.
	client.syncConversations(context.Background())

	if len(rec.events) != 0 {
		t.Errorf("queued %d events after a failed list", len(rec.events))
	}
}

func TestSyncIntervalDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{name: "unset", set: 0, want: defaultSyncInterval},
		{name: "configured", set: 5 * time.Minute, want: 5 * time.Minute},
		{name: "disabled", set: -1, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc := &NCTalkConnector{Config: Config{SyncInterval: tc.set}}
			if got := nc.syncInterval(); got != tc.want {
				t.Errorf("syncInterval() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPeriodicSyncRunsAPassImmediately(t *testing.T) {
	convs := []map[string]any{{"token": "one", "type": nctalk.RoomTypeGroup, "lastActivity": 1700000500}}
	client, rec, listCalls := newSyncClient(t, convs, "one")
	client.Main.Config.SyncInterval = time.Hour

	client.startPeriodicSync()
	t.Cleanup(client.stopPeriodicSync)

	// Talk retries no webhook, so anything sent while the bridge was down is
	// only recoverable by asking for it — waiting a whole interval to do that
	// would leave the room wrong for an hour.
	waitFor(t, func() bool { return listCalls.Load() > 0 }, "the first sync pass")
	waitFor(t, func() bool { return len(rec.recorded()) > 0 }, "the bridged room to be resynced")

	client.stopPeriodicSync()
	client.syncMu.Lock()
	stopped := client.syncCancel == nil
	client.syncMu.Unlock()
	if !stopped {
		t.Error("stopping the sync should clear its cancel function")
	}
}

func TestPeriodicSyncStaysOffWhenDisabled(t *testing.T) {
	client, _, _ := newSyncClient(t, nil)
	client.Main.Config.SyncInterval = -1

	client.startPeriodicSync()
	t.Cleanup(client.stopPeriodicSync)

	client.syncMu.Lock()
	defer client.syncMu.Unlock()
	if client.syncCancel != nil {
		t.Error("a negative interval should leave the resync loop unstarted")
	}
}

// waitFor blocks until cond holds or the test gives up.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
