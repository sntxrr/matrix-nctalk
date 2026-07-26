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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// botServer stands in for Talk's bot endpoints, verifying the request signature
// the same way spreed's ChecksumVerificationService does. signedField names the
// form field whose value is expected to be covered by the signature, which is
// the part of the protocol most easily got wrong.
func botServer(t *testing.T, signedField string, status int) (*BotClient, *recordedRequest) {
	t.Helper()

	last := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*last = recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   string(body),
		}

		random := r.Header.Get("X-Nextcloud-Talk-Bot-Random")
		signature := r.Header.Get("X-Nextcloud-Talk-Bot-Signature")
		if len(random) < minRandomLength {
			t.Errorf("random too short for Talk: %q", random)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("body is not form encoded: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		expected := sign(random, form.Get(signedField), testSecret)
		if signature != expected {
			t.Errorf("signature does not cover the %q field alone", signedField)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	return NewBotClient(srv.URL, testSecret, srv.Client()), last
}

func TestBotSendMessage(t *testing.T) {
	bot, last := botServer(t, "message", http.StatusCreated)

	if err := bot.SendMessage(context.Background(), "abc123", "hello from matrix", "", 0, false); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if !strings.HasSuffix(last.Path, "/bot/abc123/message") {
		t.Errorf("path = %q", last.Path)
	}
	if last.Header.Get("OCS-APIRequest") != "true" {
		t.Error("missing OCS-APIRequest header")
	}
	form, _ := url.ParseQuery(last.Body)
	if form.Get("message") != "hello from matrix" {
		t.Errorf("message = %q", form.Get("message"))
	}
}

func TestBotSendMessageOptionalFields(t *testing.T) {
	bot, last := botServer(t, "message", http.StatusCreated)

	if err := bot.SendMessage(context.Background(), "abc123", "a reply", "ref-123", 4711, true); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	form, _ := url.ParseQuery(last.Body)
	if form.Get("referenceId") != "ref-123" {
		t.Errorf("referenceId = %q", form.Get("referenceId"))
	}
	if form.Get("replyTo") != "4711" {
		t.Errorf("replyTo = %q", form.Get("replyTo"))
	}
	if form.Get("silent") != "true" {
		t.Errorf("silent = %q", form.Get("silent"))
	}
}

func TestBotSendMessageOmitsUnsetOptionalFields(t *testing.T) {
	bot, last := botServer(t, "message", http.StatusCreated)

	if err := bot.SendMessage(context.Background(), "abc123", "plain", "", 0, false); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	form, _ := url.ParseQuery(last.Body)
	for _, field := range []string{"referenceId", "replyTo", "silent"} {
		if form.Has(field) {
			t.Errorf("unset field %q should be omitted, got %q", field, form.Get(field))
		}
	}
}

// A message containing characters that form encoding escapes is the case where
// signing the encoded body instead of the raw text would silently break.
func TestBotSendMessageWithEncodableCharacters(t *testing.T) {
	bot, last := botServer(t, "message", http.StatusCreated)

	message := "50% & more = good + fun?"
	if err := bot.SendMessage(context.Background(), "abc123", message, "", 0, false); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	form, _ := url.ParseQuery(last.Body)
	if form.Get("message") != message {
		t.Errorf("message round-tripped wrong: %q", form.Get("message"))
	}
}

func TestBotReactions(t *testing.T) {
	bot, last := botServer(t, "reaction", http.StatusCreated)

	if err := bot.React(context.Background(), "abc123", 4711, "👍"); err != nil {
		t.Fatalf("React failed: %v", err)
	}
	if !strings.HasSuffix(last.Path, "/bot/abc123/reaction/4711") {
		t.Errorf("path = %q", last.Path)
	}
	if last.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", last.Method)
	}

	unreactBot, unreactLast := botServer(t, "reaction", http.StatusOK)
	if err := unreactBot.Unreact(context.Background(), "abc123", 4711, "👍"); err != nil {
		t.Fatalf("Unreact failed: %v", err)
	}
	if unreactLast.Method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", unreactLast.Method)
	}
}

func TestBotRequestSurfacesRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	bot := NewBotClient(srv.URL, testSecret, srv.Client())

	err := bot.SendMessage(context.Background(), "abc123", "hello", "", 0, false)
	if err == nil {
		t.Fatal("expected an error when Talk rejects the bot request")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %v", err)
	}
}

func TestBotRequestPropagatesTransportError(t *testing.T) {
	bot := NewBotClient("http://127.0.0.1:1", testSecret, http.DefaultClient)
	if err := bot.SendMessage(context.Background(), "abc123", "hello", "", 0, false); err == nil {
		t.Fatal("expected a transport error")
	}
}

func TestNewBotClientDefaults(t *testing.T) {
	bot := NewBotClient("https://cloud.example.com/", "secret", nil)
	if bot.BaseURL != "https://cloud.example.com" {
		t.Errorf("BaseURL = %q, trailing slash should be trimmed", bot.BaseURL)
	}
	if bot.HTTP == nil {
		t.Error("expected a default HTTP client")
	}
}

func TestListBots(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, []map[string]any{
			{"id": 1, "name": "Matrix Bridge", "state": BotStateEnabled, "features": BotFeaturesBridge},
			{"id": 2, "name": "Other Bot", "state": BotStateDisabled, "features": BotFeatureWebhook},
		})
	})

	bots, err := client.ListBots(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("ListBots failed: %v", err)
	}
	if !strings.HasSuffix(last.Path, "/bot/abc123") {
		t.Errorf("path = %q", last.Path)
	}
	if len(bots) != 2 || bots[0].Name != "Matrix Bridge" {
		t.Fatalf("decoded %+v", bots)
	}
}

func TestFindBotByName(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, []map[string]any{
			{"id": 1, "name": "Other Bot", "state": BotStateEnabled},
			{"id": 7, "name": "Matrix Bridge", "state": BotStateEnabled},
		})
	})

	bot, err := client.FindBotByName(context.Background(), "abc123", "Matrix Bridge")
	if err != nil {
		t.Fatalf("FindBotByName failed: %v", err)
	}
	if bot.ID != 7 {
		t.Errorf("bot ID = %d, want 7", bot.ID)
	}

	// A name mismatch is the most likely misconfiguration, so the error must
	// name what was being looked for.
	_, err = client.FindBotByName(context.Background(), "abc123", "Wrong Name")
	if err == nil {
		t.Fatal("expected an error for an absent bot")
	}
	if !strings.Contains(err.Error(), "Wrong Name") {
		t.Errorf("error should name the missing bot, got %v", err)
	}
}

func TestFindBotByNamePropagatesListError(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Talk returns 403 when the caller is not a moderator.
		writeOCSError(w, http.StatusOK, http.StatusForbidden, "Not a moderator")
	})
	if _, err := client.FindBotByName(context.Background(), "abc123", "Matrix Bridge"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestEnableAndDisableBot(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": 7, "name": "Matrix Bridge", "state": BotStateEnabled})
	})

	if err := client.EnableBot(context.Background(), "abc123", 7); err != nil {
		t.Fatalf("EnableBot failed: %v", err)
	}
	if last.Method != http.MethodPost || !strings.HasSuffix(last.Path, "/bot/abc123/7") {
		t.Errorf("enable request = %s %s", last.Method, last.Path)
	}

	if err := client.DisableBot(context.Background(), "abc123", 7); err != nil {
		t.Fatalf("DisableBot failed: %v", err)
	}
	if last.Method != http.MethodDelete {
		t.Errorf("disable method = %q, want DELETE", last.Method)
	}
}

// Talk returns 400 for a bot installed with --no-setup, for federated
// conversations, and for former one-to-one conversations.
func TestEnableBotSurfacesRejection(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusBadRequest, http.StatusBadRequest, "bot")
	})

	err := client.EnableBot(context.Background(), "abc123", 7)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "abc123") {
		t.Errorf("error should name the conversation, got %v", err)
	}
}
