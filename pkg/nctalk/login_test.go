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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartLoginFlow(t *testing.T) {
	var gotPath, gotUA, srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		// A real Nextcloud names itself here. A server that named somewhere
		// else would be refused; TestStartLoginFlowRejectsForeignEndpoints
		// covers that.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"poll":  map[string]string{"token": "polltoken", "endpoint": srvURL + "/index.php/login/v2/poll"},
			"login": srvURL + "/index.php/login/v2/flow/abc",
		})
	}))
	defer srv.Close()
	srvURL = srv.URL

	init, err := StartLoginFlow(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("StartLoginFlow failed: %v", err)
	}
	if gotPath != "/index.php/login/v2" {
		t.Errorf("path = %q", gotPath)
	}
	// Nextcloud names the created app password after the User-Agent, so it
	// shows up in the user's security settings.
	if gotUA != UserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, UserAgent)
	}
	if init.Poll.Token != "polltoken" || init.Login == "" {
		t.Errorf("decoded %+v", init)
	}
}

func TestStartLoginFlowRejectsIncompleteHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Missing the login URL.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"poll": map[string]string{"token": "polltoken", "endpoint": "https://example.com/poll"},
		})
	}))
	defer srv.Close()

	if _, err := StartLoginFlow(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("expected an error for an incomplete handshake")
	}
}

func TestStartLoginFlowPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := StartLoginFlow(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}

// Nextcloud answers the poll endpoint with 404 until the user approves the
// request in their browser. Treating that as a failure would break every login.
func TestPollLoginReportsPendingFor404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	init := &LoginFlowInit{}
	init.Poll.Token = "polltoken"
	init.Poll.Endpoint = srv.URL

	_, err := PollLogin(context.Background(), srv.Client(), init)
	if !errors.Is(err, ErrLoginPending) {
		t.Fatalf("expected ErrLoginPending, got %v", err)
	}
}

func TestPollLoginReturnsCredentials(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"server":      "https://cloud.example.com",
			"loginName":   "alice",
			"appPassword": "generated-app-password",
		})
	}))
	defer srv.Close()

	init := &LoginFlowInit{}
	init.Poll.Token = "polltoken"
	init.Poll.Endpoint = srv.URL

	res, err := PollLogin(context.Background(), srv.Client(), init)
	if err != nil {
		t.Fatalf("PollLogin failed: %v", err)
	}
	if !strings.Contains(gotBody, "token=polltoken") {
		t.Errorf("poll body = %q, want the poll token", gotBody)
	}
	if res.LoginName != "alice" || res.AppPassword != "generated-app-password" {
		t.Errorf("decoded %+v", res)
	}
}

func TestPollLoginRejectsIncompleteCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"server": "https://cloud.example.com", "loginName": "alice"})
	}))
	defer srv.Close()

	init := &LoginFlowInit{}
	init.Poll.Endpoint = srv.URL

	if _, err := PollLogin(context.Background(), srv.Client(), init); err == nil {
		t.Fatal("expected an error when no app password is returned")
	}
}

func TestWaitForLoginPollsUntilApproved(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"server": "https://cloud.example.com", "loginName": "alice", "appPassword": "pw",
		})
	}))
	defer srv.Close()

	init := &LoginFlowInit{}
	init.Poll.Endpoint = srv.URL

	res, err := WaitForLogin(context.Background(), srv.Client(), init, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForLogin failed: %v", err)
	}
	if res.AppPassword != "pw" {
		t.Errorf("app password = %q", res.AppPassword)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("expected at least 3 poll attempts, got %d", got)
	}
}

func TestWaitForLoginStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	init := &LoginFlowInit{}
	init.Poll.Endpoint = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := WaitForLogin(ctx, srv.Client(), init, time.Millisecond); err == nil {
		t.Fatal("expected an error when the context expires")
	}
}

func TestWaitForLoginStopsOnFatalError(t *testing.T) {
	// A non-404 failure means something is actually wrong; retrying forever
	// would hide it behind the timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	init := &LoginFlowInit{}
	init.Poll.Endpoint = srv.URL

	done := make(chan error, 1)
	go func() {
		_, err := WaitForLogin(context.Background(), srv.Client(), init, time.Millisecond)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, ErrLoginPending) {
			t.Fatal("a 500 should not be treated as pending")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForLogin retried a fatal error instead of returning")
	}
}

func TestVerifyCredentials(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ocs/v2.php/cloud/capabilities":
			writeOCS(t, w, map[string]any{
				"capabilities": map[string]any{
					"spreed": map[string]any{"features": []string{CapBotsV1}},
				},
			})
		case "/ocs/v2.php/cloud/user":
			writeOCS(t, w, map[string]any{"id": "alice", "displayname": "Alice Example", "email": "a@example.com"})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})

	me, caps, err := client.VerifyCredentials(context.Background())
	if err != nil {
		t.Fatalf("VerifyCredentials failed: %v", err)
	}
	if me.ID != "alice" || me.DisplayName != "Alice Example" {
		t.Errorf("user decoded %+v", me)
	}
	if !caps.Has(CapBotsV1) {
		t.Error("capabilities were not returned")
	}
}

func TestVerifyCredentialsFailsOnBadPassword(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, _, err := client.VerifyCredentials(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsUnauthorized(err) {
		t.Errorf("expected an unauthorized error, got %v", err)
	}
}

func TestVerifyCredentialsRejectsMissingUserID(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ocs/v2.php/cloud/capabilities":
			writeOCS(t, w, map[string]any{"capabilities": map[string]any{"spreed": map[string]any{"features": []string{}}}})
		default:
			writeOCS(t, w, map[string]any{"displayname": "No ID"})
		}
	})

	if _, _, err := client.VerifyCredentials(context.Background()); err == nil {
		t.Fatal("expected an error when the server returns no user ID")
	}
}

func TestStartLoginFlowRejectsForeignEndpoints(t *testing.T) {
	// A hostile server answering the handshake can name any URL it likes. The
	// poll endpoint is the dangerous one — the bridge posts to it every couple
	// of seconds until the login times out — so pointing it at an internal
	// address would turn one typed URL into a sustained request generator.
	for _, tc := range []struct {
		name string
		poll string
		show string
	}{
		{
			name: "poll endpoint aimed at cloud metadata",
			poll: "http://169.254.169.254/latest/meta-data/",
			show: "%s/index.php/login/v2/flow/abc",
		},
		{
			name: "login URL aimed at a phishing page",
			poll: "%s/index.php/login/v2/poll",
			show: "https://not-your-nextcloud.example/login",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var srvURL string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sub := func(s string) string {
					if strings.Contains(s, "%s") {
						return fmt.Sprintf(s, srvURL)
					}
					return s
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"poll":  map[string]string{"token": "polltoken", "endpoint": sub(tc.poll)},
					"login": sub(tc.show),
				})
			}))
			defer srv.Close()
			srvURL = srv.URL

			_, err := StartLoginFlow(context.Background(), srv.Client(), srv.URL)
			if err == nil {
				t.Fatal("the handshake was accepted")
			}
			if !errors.Is(err, ErrForeignLoginEndpoint) {
				t.Errorf("error %v does not identify the cause", err)
			}
		})
	}
}

func TestPollLoginRechecksTheEndpoint(t *testing.T) {
	// An init assembled by hand, or mutated after the handshake, must not get
	// a free pass just because StartLoginFlow already looked.
	init := &LoginFlowInit{Server: "https://cloud.example.com"}
	init.Poll.Token = "polltoken"
	init.Poll.Endpoint = "http://169.254.169.254/latest/meta-data/"

	_, err := PollLogin(context.Background(), http.DefaultClient, init)
	if !errors.Is(err, ErrForeignLoginEndpoint) {
		t.Errorf("PollLogin error = %v, want it refused as foreign", err)
	}
}
