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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

func TestValidateServerURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain https", in: "https://cloud.example.com", want: "https://cloud.example.com"},
		{name: "trailing slash trimmed", in: "https://cloud.example.com/", want: "https://cloud.example.com"},
		{name: "surrounding whitespace", in: "  https://cloud.example.com  ", want: "https://cloud.example.com"},
		// A bare hostname is what most people will type, so it must be accepted
		// and upgraded rather than rejected.
		{name: "bare host gets https", in: "cloud.example.com", want: "https://cloud.example.com"},
		{name: "subpath install", in: "https://example.com/nextcloud", want: "https://example.com/nextcloud"},
		{name: "explicit port", in: "https://cloud.example.com:8443", want: "https://cloud.example.com:8443"},
		// http is permitted for local development, though it is a poor idea in
		// production since it carries the app password.
		{name: "http allowed", in: "http://localhost:8080", want: "http://localhost:8080"},

		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   ", wantErr: true},
		{name: "unsupported scheme", in: "ftp://cloud.example.com", wantErr: true},
		{name: "scheme with no host", in: "https://", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateServerURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateServerURL(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateServerURL(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("validateServerURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateServerURLIsIdempotent(t *testing.T) {
	// The validator runs both client-side and again on submit, so a second
	// pass over its own output must not change the result.
	for _, in := range []string{"cloud.example.com", "https://cloud.example.com/", "https://example.com/nextcloud"} {
		once, err := validateServerURL(in)
		if err != nil {
			t.Fatalf("first pass for %q failed: %v", in, err)
		}
		twice, err := validateServerURL(once)
		if err != nil {
			t.Fatalf("second pass for %q failed: %v", once, err)
		}
		if once != twice {
			t.Errorf("not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func TestHostOf(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "https://cloud.example.com", want: "cloud.example.com"},
		{in: "https://cloud.example.com:8443", want: "cloud.example.com:8443"},
		{in: "https://example.com/nextcloud", want: "example.com"},
		{in: "http://localhost:8080", want: "localhost:8080"},
		{in: "", wantErr: true},
		{in: "not-a-url", wantErr: true},
	}
	for _, tc := range tests {
		got, err := hostOf(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("hostOf(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("hostOf(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("hostOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The host from hostOf becomes the prefix of every network ID, so it must agree
// with what the OCS client reports for the same server.
func TestHostOfAgreesWithClientHost(t *testing.T) {
	for _, in := range []string{"https://cloud.example.com", "https://cloud.example.com:8443", "https://example.com/nextcloud"} {
		fromLogin, err := hostOf(in)
		if err != nil {
			t.Fatalf("hostOf(%q) failed: %v", in, err)
		}
		fromClient := nctalk.NewClient(in, "", "").Host()
		if fromLogin != fromClient {
			t.Errorf("host mismatch for %q: login says %q, client says %q", in, fromLogin, fromClient)
		}
	}
}

func TestGetLoginFlows(t *testing.T) {
	nc := &NCTalkConnector{}
	flows := nc.GetLoginFlows()
	if len(flows) != 2 {
		t.Fatalf("got %d flows, want 2", len(flows))
	}

	ids := make(map[string]bool, len(flows))
	for _, f := range flows {
		if f.ID == "" || f.Name == "" || f.Description == "" {
			t.Errorf("flow %+v has empty fields", f)
		}
		ids[f.ID] = true
	}
	if !ids[LoginFlowIDBrowser] || !ids[LoginFlowIDAppPassword] {
		t.Errorf("expected both login flows, got %v", ids)
	}
}

func TestCreateLoginRejectsUnknownFlow(t *testing.T) {
	nc := &NCTalkConnector{}

	for _, id := range []string{LoginFlowIDBrowser, LoginFlowIDAppPassword} {
		proc, err := nc.CreateLogin(t.Context(), nil, id)
		if err != nil {
			t.Errorf("CreateLogin(%q) failed: %v", id, err)
		}
		if proc == nil {
			t.Errorf("CreateLogin(%q) returned no process", id)
		}
	}

	if _, err := nc.CreateLogin(t.Context(), nil, "telepathy"); err == nil {
		t.Error("expected an error for an unknown flow ID")
	}
}

func TestLoginStartAsksForServerURL(t *testing.T) {
	l := &NCTalkLogin{Main: &NCTalkConnector{}, FlowID: LoginFlowIDBrowser}
	step, err := l.Start(t.Context())
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if step.Type != "user_input" {
		t.Errorf("step type = %q, want user_input", step.Type)
	}
	if step.UserInputParams == nil || len(step.UserInputParams.Fields) != 1 {
		t.Fatalf("expected exactly one input field, got %+v", step.UserInputParams)
	}
	field := step.UserInputParams.Fields[0]
	if field.ID != "server_url" {
		t.Errorf("field ID = %q, want server_url", field.ID)
	}
	if field.Validate == nil {
		t.Error("the server URL field should carry a validator")
	}
}

func TestLoginRejectsDisallowedServer(t *testing.T) {
	nc := &NCTalkConnector{Config: Config{AllowedServers: []string{"cloud.example.com"}}}
	l := &NCTalkLogin{Main: nc, FlowID: LoginFlowIDAppPassword}

	_, err := l.SubmitUserInput(t.Context(), map[string]string{"server_url": "https://evil.example.net"})
	if err == nil {
		t.Fatal("expected a disallowed server to be rejected")
	}
	if l.serverURL != "" && l.serverURL != "https://evil.example.net" {
		t.Errorf("unexpected stored server URL %q", l.serverURL)
	}
}

func TestLoginAppPasswordFlowAsksForCredentials(t *testing.T) {
	nc := &NCTalkConnector{}
	l := &NCTalkLogin{Main: nc, FlowID: LoginFlowIDAppPassword}

	step, err := l.SubmitUserInput(t.Context(), map[string]string{"server_url": "cloud.example.com"})
	if err != nil {
		t.Fatalf("SubmitUserInput failed: %v", err)
	}
	if step.StepID != loginStepCredentials {
		t.Fatalf("step = %q, want the credentials step", step.StepID)
	}
	if l.serverURL != "https://cloud.example.com" {
		t.Errorf("server URL was not normalised, got %q", l.serverURL)
	}

	fields := map[string]bool{}
	for _, f := range step.UserInputParams.Fields {
		fields[f.ID] = true
	}
	if !fields["username"] || !fields["app_password"] {
		t.Errorf("expected username and app_password fields, got %v", fields)
	}
}

func TestLoginRejectsUnexpectedInput(t *testing.T) {
	l := &NCTalkLogin{Main: &NCTalkConnector{}, FlowID: LoginFlowIDAppPassword, serverURL: "https://cloud.example.com"}
	if _, err := l.SubmitUserInput(t.Context(), map[string]string{"nonsense": "value"}); err == nil {
		t.Error("expected an error for unrecognised input")
	}
}

func TestLoginWaitRequiresStartedHandshake(t *testing.T) {
	l := &NCTalkLogin{Main: &NCTalkConnector{}, FlowID: LoginFlowIDBrowser}
	if _, err := l.Wait(t.Context()); err == nil {
		t.Error("expected an error when Wait is called before the handshake starts")
	}
}

func TestLoginCancelIsSafeBeforeStart(t *testing.T) {
	l := &NCTalkLogin{Main: &NCTalkConnector{}}
	l.Cancel() // must not panic when there is nothing to cancel
}

func TestLoginTimeoutDefault(t *testing.T) {
	nc := &NCTalkConnector{}
	if nc.loginTimeout() <= 0 {
		t.Error("expected a positive default login timeout")
	}

	nc.Config.LoginTimeout = 42
	if nc.loginTimeout() != 42 {
		t.Errorf("configured timeout was not used, got %v", nc.loginTimeout())
	}
}

// A pasted URL may carry a query or fragment; those must not survive into the
// base URL that every OCS path is appended to.
func TestValidateServerURLStripsQueryAndFragment(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://cloud.example.com/?foo=bar", "https://cloud.example.com"},
		{"https://cloud.example.com/#section", "https://cloud.example.com"},
		{"https://example.com/nextcloud/?a=1#b", "https://example.com/nextcloud"},
	}
	for _, tc := range tests {
		got, err := validateServerURL(tc.in)
		if err != nil {
			t.Errorf("validateServerURL(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("validateServerURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStartBrowserFlowPresentsApprovalURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"poll":  map[string]string{"token": "polltoken", "endpoint": "https://cloud.example.com/poll"},
			"login": "https://cloud.example.com/index.php/login/v2/flow/abc",
		})
	}))
	defer srv.Close()

	nc := &NCTalkConnector{HTTP: srv.Client()}
	l := &NCTalkLogin{Main: nc, FlowID: LoginFlowIDBrowser}

	step, err := l.SubmitUserInput(t.Context(), map[string]string{"server_url": srv.URL})
	if err != nil {
		t.Fatalf("SubmitUserInput failed: %v", err)
	}
	if step.StepID != loginStepBrowserWait {
		t.Fatalf("step = %q, want the browser wait step", step.StepID)
	}
	if step.DisplayAndWaitParams == nil {
		t.Fatal("expected display-and-wait params")
	}
	// The user has to be able to reach the approval page, so the URL must be
	// both in the data and visible in the instructions.
	if step.DisplayAndWaitParams.Data != "https://cloud.example.com/index.php/login/v2/flow/abc" {
		t.Errorf("data = %q", step.DisplayAndWaitParams.Data)
	}
	if !strings.Contains(step.Instructions, "https://cloud.example.com/index.php/login/v2/flow/abc") {
		t.Errorf("instructions do not show the approval URL: %q", step.Instructions)
	}
}

func TestStartBrowserFlowSurfacesUnreachableServer(t *testing.T) {
	nc := &NCTalkConnector{HTTP: http.DefaultClient}
	l := &NCTalkLogin{Main: nc, FlowID: LoginFlowIDBrowser}

	if _, err := l.SubmitUserInput(t.Context(), map[string]string{"server_url": "http://127.0.0.1:1"}); err == nil {
		t.Fatal("expected an error when the server cannot be reached")
	}
}

func TestWaitTimesOutWaitingForApproval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never approved.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	nc := &NCTalkConnector{HTTP: srv.Client(), Config: Config{LoginTimeout: 50 * time.Millisecond}}
	l := &NCTalkLogin{Main: nc, FlowID: LoginFlowIDBrowser, serverURL: srv.URL}
	l.flowInit = &nctalk.LoginFlowInit{}
	l.flowInit.Poll.Endpoint = srv.URL
	l.flowInit.Poll.Token = "polltoken"

	_, err := l.Wait(t.Context())
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention the timeout, got %v", err)
	}
}

func TestFinishRejectsMissingCredentials(t *testing.T) {
	l := &NCTalkLogin{Main: &NCTalkConnector{}, serverURL: "https://cloud.example.com"}

	if _, err := l.finish(t.Context(), "", "pw"); err == nil {
		t.Error("expected an error with no username")
	}
	if _, err := l.finish(t.Context(), "alice", ""); err == nil {
		t.Error("expected an error with no app password")
	}
}

func TestFinishRejectsBadCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	nc := &NCTalkConnector{HTTP: srv.Client()}
	l := &NCTalkLogin{Main: nc, serverURL: srv.URL}

	_, err := l.finish(t.Context(), "alice", "wrong-password")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error should say the credentials were rejected, got %v", err)
	}
}

// A Talk too old for the bot API cannot be bridged at all, so this must fail at
// login rather than after the user thinks they are set up.
func TestFinishRejectsServerWithoutBotSupport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ocs/v2.php/cloud/capabilities":
			_, _ = w.Write([]byte(`{"ocs":{"meta":{"status":"ok","statuscode":200},"data":{"capabilities":{"spreed":{"features":["chat-v2"]}}}}}`))
		default:
			_, _ = w.Write([]byte(`{"ocs":{"meta":{"status":"ok","statuscode":200},"data":{"id":"alice","displayname":"Alice"}}}`))
		}
	}))
	defer srv.Close()

	nc := &NCTalkConnector{HTTP: srv.Client()}
	l := &NCTalkLogin{Main: nc, serverURL: srv.URL}

	_, err := l.finish(t.Context(), "alice", "pw")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "bots") {
		t.Errorf("error should explain that bots are unsupported, got %v", err)
	}
}

func TestCancelStopsAnInFlightWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	nc := &NCTalkConnector{HTTP: srv.Client(), Config: Config{LoginTimeout: time.Minute}}
	l := &NCTalkLogin{Main: nc, FlowID: LoginFlowIDBrowser, serverURL: srv.URL}
	l.flowInit = &nctalk.LoginFlowInit{}
	l.flowInit.Poll.Endpoint = srv.URL

	done := make(chan struct{})
	go func() {
		_, _ = l.Wait(t.Context())
		close(done)
	}()

	// Give Wait a moment to install its cancel function, then cancel it.
	// Read through the mutex: Wait writes l.cancel from the other goroutine.
	deadline := time.After(2 * time.Second)
	for {
		l.cancelMu.Lock()
		installed := l.cancel != nil
		l.cancelMu.Unlock()
		if installed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Wait never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	l.Cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel did not stop the in-flight Wait")
	}
}
