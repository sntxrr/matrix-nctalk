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

import "testing"

func TestUserLoginIDRoundTrip(t *testing.T) {
	id := makeUserLoginID("cloud.example.com", "alice")
	host, user, err := parseUserLoginID(id)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if host != "cloud.example.com" || user != "alice" {
		t.Fatalf("round trip lost data: host=%q user=%q", host, user)
	}
}

func TestUserIDRoundTrip(t *testing.T) {
	// Nextcloud user IDs may contain characters that also appear in the
	// separator scheme, so the actor ID must be parsed as the remainder.
	for _, actorID := range []string{"alice", "alice:bob", "user@example.com", "a:b:c"} {
		id := makeUserID("cloud.example.com", "users", actorID)
		host, actorType, got, err := parseUserID(id)
		if err != nil {
			t.Fatalf("parse %q failed: %v", id, err)
		}
		if host != "cloud.example.com" || actorType != "users" || got != actorID {
			t.Fatalf("round trip lost data for %q: host=%q type=%q id=%q", actorID, host, actorType, got)
		}
	}
}

func TestPortalIDRoundTrip(t *testing.T) {
	id := makePortalID("cloud.example.com", "abc123token")
	host, token, err := parsePortalID(id)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if host != "cloud.example.com" || token != "abc123token" {
		t.Fatalf("round trip lost data: host=%q token=%q", host, token)
	}
}

func TestPortalKeyHasEmptyReceiver(t *testing.T) {
	// Talk conversation tokens are global to the server, so all bridged users
	// of a conversation must share one portal rather than each getting a copy.
	key := makePortalKey("cloud.example.com", "abc123token")
	if key.Receiver != "" {
		t.Fatalf("expected empty receiver for shared portal, got %q", key.Receiver)
	}
}

func TestMessageIDRoundTrip(t *testing.T) {
	id := makeMessageID("cloud.example.com", "abc123token", 4711)
	host, token, msgID, err := parseMessageID(id)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if host != "cloud.example.com" || token != "abc123token" || msgID != 4711 {
		t.Fatalf("round trip lost data: host=%q token=%q id=%d", host, token, msgID)
	}
}

func TestParseMalformedIDsError(t *testing.T) {
	if _, _, err := parseUserLoginID("nocolon"); err == nil {
		t.Error("expected error for login ID without separator")
	}
	if _, _, _, err := parseUserID("only|two"); err == nil {
		t.Error("expected error for user ID with too few parts")
	}
	if _, _, err := parsePortalID("|emptyhost"); err == nil {
		t.Error("expected error for portal ID with empty host")
	}
	if _, _, _, err := parseMessageID("host|token|notanumber"); err == nil {
		t.Error("expected error for non-numeric message ID")
	}
}

func TestParseTalkActor(t *testing.T) {
	tests := []struct {
		in        string
		wantType  string
		wantID    string
		wantError bool
	}{
		{in: "users/alice", wantType: "users", wantID: "alice"},
		{in: "bots/bot-3f9ade", wantType: "bots", wantID: "bot-3f9ade"},
		{in: "guests/2a1b3c", wantType: "guests", wantID: "2a1b3c"},
		{in: "federated_users/bob@other.example", wantType: "federated_users", wantID: "bob@other.example"},
		{in: "malformed", wantError: true},
		{in: "/alice", wantError: true},
		{in: "users/", wantError: true},
	}
	for _, tc := range tests {
		actorType, id, err := parseTalkActor(tc.in)
		if tc.wantError {
			if err == nil {
				t.Errorf("parseTalkActor(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTalkActor(%q): unexpected error %v", tc.in, err)
			continue
		}
		if actorType != tc.wantType || id != tc.wantID {
			t.Errorf("parseTalkActor(%q) = (%q, %q), want (%q, %q)", tc.in, actorType, id, tc.wantType, tc.wantID)
		}
	}
}

func TestServerAllowed(t *testing.T) {
	empty := &Config{}
	if !empty.ServerAllowed("anything.example") {
		t.Error("empty allowlist should permit any server")
	}

	restricted := &Config{AllowedServers: []string{"cloud.example.com", " Other.Example.Org "}}
	if !restricted.ServerAllowed("cloud.example.com") {
		t.Error("listed server rejected")
	}
	if !restricted.ServerAllowed("other.example.org") {
		t.Error("allowlist should be case-insensitive and whitespace-tolerant")
	}
	if restricted.ServerAllowed("evil.example.net") {
		t.Error("unlisted server allowed")
	}
}

// A Nextcloud reachable on a non-standard port, or over IPv6, puts colons in the
// host. Splitting IDs on a colon silently yields the wrong host, so every ID
// type must survive these.
func TestIDsRoundTripWithColonsInHost(t *testing.T) {
	hosts := []string{
		"cloud.example.com",
		"cloud.example.com:8443",
		"localhost:8080",
		"[::1]:8443",
		"[2001:db8::1]:443",
	}
	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			gotHost, gotUser, err := parseUserLoginID(makeUserLoginID(host, "alice"))
			if err != nil || gotHost != host || gotUser != "alice" {
				t.Errorf("login ID: host=%q user=%q err=%v", gotHost, gotUser, err)
			}

			gotHost, actorType, actorID, err := parseUserID(makeUserID(host, "users", "alice"))
			if err != nil || gotHost != host || actorType != "users" || actorID != "alice" {
				t.Errorf("user ID: host=%q type=%q id=%q err=%v", gotHost, actorType, actorID, err)
			}

			gotHost, token, err := parsePortalID(makePortalID(host, "abc123"))
			if err != nil || gotHost != host || token != "abc123" {
				t.Errorf("portal ID: host=%q token=%q err=%v", gotHost, token, err)
			}

			gotHost, token, msgID, err := parseMessageID(makeMessageID(host, "abc123", 4711))
			if err != nil || gotHost != host || token != "abc123" || msgID != 4711 {
				t.Errorf("message ID: host=%q token=%q id=%d err=%v", gotHost, token, msgID, err)
			}
		})
	}
}

// Nextcloud user IDs are permissive, so the trailing free-form field must
// survive even if it contains the separator itself.
func TestIDsRoundTripWithSeparatorInUserID(t *testing.T) {
	odd := "weird|user"

	host, user, err := parseUserLoginID(makeUserLoginID("cloud.example.com", odd))
	if err != nil || host != "cloud.example.com" || user != odd {
		t.Errorf("login ID: host=%q user=%q err=%v", host, user, err)
	}

	host, actorType, actorID, err := parseUserID(makeUserID("cloud.example.com", "users", odd))
	if err != nil || host != "cloud.example.com" || actorType != "users" || actorID != odd {
		t.Errorf("user ID: host=%q type=%q id=%q err=%v", host, actorType, actorID, err)
	}
}
