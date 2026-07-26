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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// fakeLogins is a stand-in for the bridge's login cache.
type fakeLogins struct {
	logins []*bridgev2.UserLogin
}

func (f *fakeLogins) GetAllCachedUserLogins() []*bridgev2.UserLogin {
	return f.logins
}

// recordedRequest captures what the client sent, for asserting on paths.
type recordedRequest struct {
	Method string
	Path   string
	Body   string
}

// newOCSServer starts a Nextcloud stand-in and returns its base URL.
func newOCSServer(t *testing.T, handler http.HandlerFunc) (string, *recordedRequest) {
	t.Helper()
	last := &recordedRequest{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*last = recordedRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)}
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, last
}

// writeOCS writes a successful OCS envelope around data.
func writeOCS(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal test data: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ocs":{"meta":{"status":"ok","statuscode":200,"message":"OK"},"data":` + string(raw) + `}}`))
}

// writeOCSError writes an OCS failure envelope.
func writeOCSError(w http.ResponseWriter, ocsStatus int, message string) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := json.Marshal(map[string]any{
		"ocs": map[string]any{
			"meta": map[string]any{"status": "failure", "statuscode": ocsStatus, "message": message},
			"data": nil,
		},
	})
	_, _ = w.Write(body)
}

// newTestClient builds an NCTalkClient wired to serverURL, without a Bridge.
// Anything that queues remote events needs a real bridge and is not covered
// through this helper.
func newTestClient(t *testing.T, serverURL, username string, cfg Config) *NCTalkClient {
	t.Helper()

	main := &NCTalkConnector{
		Bridge: &bridgev2.Bridge{Log: zerolog.Nop()},
		Config: cfg,
		HTTP:   http.DefaultClient,
	}
	main.router = newLoginRouter(&fakeLogins{})

	client := nctalk.NewClient(serverURL, username, "app-password")
	host := client.Host()

	login := &bridgev2.UserLogin{
		UserLogin: &database.UserLogin{
			ID: makeUserLoginID(host, username),
			Metadata: &UserLoginMetadata{
				ServerURL:   serverURL,
				Username:    username,
				AppPassword: "app-password",
			},
		},
		Log: zerolog.Nop(),
	}

	nc := &NCTalkClient{
		Main:      main,
		UserLogin: login,
		Client:    client,
		Bot:       nctalk.NewBotClient(serverURL, cfg.BotSecret, http.DefaultClient),
	}
	login.Client = nc
	return nc
}

// newTestPortal builds a Portal carrying bridge metadata for the given token.
func newTestPortal(host, token string) *bridgev2.Portal {
	return &bridgev2.Portal{
		Portal: &database.Portal{
			PortalKey: makePortalKey(host, token),
			Metadata:  &PortalMetadata{},
		},
	}
}

// newTestGhost builds a Ghost for the given Talk actor.
func newTestGhost(host, actorType, actorID string) *bridgev2.Ghost {
	return &bridgev2.Ghost{
		Ghost: &database.Ghost{
			ID:       makeUserID(host, actorType, actorID),
			Metadata: &GhostMetadata{ActorType: actorType},
		},
	}
}

// newQuietBridge returns a Bridge with only the logger populated, for code
// paths that log but do not touch the database or Matrix.
func newQuietBridge() *bridgev2.Bridge {
	return &bridgev2.Bridge{Log: zerolog.Nop()}
}

// fakeGhostParser stands in for the Matrix connector's ghost MXID parsing, which
// is all the outgoing mention converter needs from the bridge.
type fakeGhostParser struct {
	ghosts map[id.UserID]networkid.UserID
}

func (f *fakeGhostParser) ParseGhostMXID(mxid id.UserID) (networkid.UserID, bool) {
	ghost, ok := f.ghosts[mxid]
	return ghost, ok
}

// newTestMatrixMessage builds the outgoing Matrix event bridgev2 would hand to
// HandleMatrixMessage.
func newTestMatrixMessage(portal *bridgev2.Portal, content *event.MessageEventContent) *bridgev2.MatrixMessage {
	return &bridgev2.MatrixMessage{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{
			Event: &event.Event{
				ID:        "$event1:matrix.example.com",
				Sender:    "@alice:matrix.example.com",
				Timestamp: 1700000000000,
				RoomID:    "!room:matrix.example.com",
			},
			Content: content,
			Portal:  portal,
		},
	}
}

// newTestTargetMessage builds a stored message an outgoing event can point at,
// as bridgev2 resolves reply and thread targets before calling the connector.
func newTestTargetMessage(id networkid.MessageID) *database.Message {
	return &database.Message{ID: id}
}
