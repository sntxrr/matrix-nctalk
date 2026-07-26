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
