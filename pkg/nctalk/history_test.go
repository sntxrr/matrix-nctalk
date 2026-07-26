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
	"net/http"
	"net/url"
	"testing"
)

func TestGetHistoryReadsBackwards(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderChatLastGiven, "135")
		writeOCS(t, w, []Message{{ID: 139}, {ID: 137}, {ID: 135}})
	})

	page, err := client.GetHistory(context.Background(), HistoryRequest{
		Token:              "abc123token",
		LastKnownMessageID: 140,
		Limit:              3,
	})
	if err != nil {
		t.Fatalf("GetHistory failed: %v", err)
	}
	if len(page.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(page.Messages))
	}
	if page.LastGiven != 135 {
		t.Errorf("LastGiven = %d, want the header value", page.LastGiven)
	}
	// The token is what makes a message addressable later; the caller knows it
	// even when the payload leaves it out.
	if page.Messages[0].Token != "abc123token" {
		t.Errorf("message token = %q, want it filled in from the request", page.Messages[0].Token)
	}

	query, err := url.ParseQuery(last.Query)
	if err != nil {
		t.Fatalf("could not parse the query the client sent: %v", err)
	}
	want := map[string]string{
		"lookIntoFuture":     "0",
		"lastKnownMessageId": "140",
		"limit":              "3",
		"includeLastKnown":   "0",
		// Reading history is the bridge's doing, not the user's, so it must not
		// show up on the Talk side as them reading their chat.
		"setReadMarker":           "0",
		"noStatusUpdate":          "1",
		"markNotificationsAsRead": "0",
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Errorf("query %s = %q, want %q", key, got, value)
		}
	}
	if query.Has("timeout") {
		t.Error("a backwards read should not ask for a long poll timeout")
	}
}

func TestGetHistoryReadsForwards(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderChatLastGiven, "145")
		writeOCS(t, w, []Message{{ID: 141}, {ID: 145}})
	})

	page, err := client.GetHistory(context.Background(), HistoryRequest{
		Token:              "abc123token",
		LastKnownMessageID: 140,
		Limit:              10,
		Future:             true,
	})
	if err != nil {
		t.Fatalf("GetHistory failed: %v", err)
	}
	if page.LastGiven != 145 {
		t.Errorf("LastGiven = %d, want 145", page.LastGiven)
	}

	query, _ := url.ParseQuery(last.Query)
	if query.Get("lookIntoFuture") != "1" {
		t.Errorf("lookIntoFuture = %q, want 1", query.Get("lookIntoFuture"))
	}
	// A long poll per conversation is the design the bot webhook exists to avoid.
	if query.Get("timeout") != "0" {
		t.Errorf("timeout = %q, want 0 so the call returns immediately", query.Get("timeout"))
	}
}

func TestGetHistoryClampsLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		want  string
	}{
		// Talk rejects a larger limit outright with a bodyless 400 rather than
		// clamping it the way its own controller does.
		{name: "above the maximum", limit: 500, want: "200"},
		{name: "zero", limit: 0, want: "1"},
		{name: "negative", limit: -5, want: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeOCS(t, w, []Message{})
			})
			_, err := client.GetHistory(context.Background(), HistoryRequest{Token: "abc123token", Limit: tc.limit})
			if err != nil {
				t.Fatalf("GetHistory failed: %v", err)
			}
			query, _ := url.ParseQuery(last.Query)
			if got := query.Get("limit"); got != tc.want {
				t.Errorf("limit = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGetHistoryTreatsNotModifiedAsTheEnd(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Talk answers a chat request with nothing to return this way, with no
		// body at all, so there is no OCS envelope to read.
		w.WriteHeader(http.StatusNotModified)
	})

	page, err := client.GetHistory(context.Background(), HistoryRequest{Token: "abc123token", Limit: 10})
	if err != nil {
		t.Fatalf("a 304 is the end of the history, not an error: %v", err)
	}
	if len(page.Messages) != 0 || page.LastGiven != 0 {
		t.Errorf("got %d messages and cursor %d, want an empty page", len(page.Messages), page.LastGiven)
	}
}

func TestGetHistoryReportsFailure(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusNotFound, http.StatusNotFound, "conversation not found")
	})

	_, err := client.GetHistory(context.Background(), HistoryRequest{Token: "gone", Limit: 10})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsNotFound(err) {
		t.Errorf("error %v should be recognisable as a 404", err)
	}
}

func TestRequestReportsBodylessFailure(t *testing.T) {
	// Nextcloud rejects an out-of-range query parameter before the OCS layer
	// gets involved, so the reply carries a status and nothing else.
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})

	_, err := client.request(context.Background(), http.MethodGet, "/test", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsBadRequest(err) {
		t.Errorf("error %v should be recognisable as a 400", err)
	}
}
