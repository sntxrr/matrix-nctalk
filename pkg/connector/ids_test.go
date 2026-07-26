package connector

import "testing"

func TestUserLoginIDRoundTrip(t *testing.T) {
	id := makeUserLoginID("cloud.example.com", "alice")
	if id != "cloud.example.com:alice" {
		t.Fatalf("unexpected login ID %q", id)
	}
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
	if _, _, _, err := parseUserID("only:two"); err == nil {
		t.Error("expected error for user ID with too few parts")
	}
	if _, _, err := parsePortalID(":emptyhost"); err == nil {
		t.Error("expected error for portal ID with empty host")
	}
	if _, _, _, err := parseMessageID("host:token:notanumber"); err == nil {
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
