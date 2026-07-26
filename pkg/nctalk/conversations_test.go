package nctalk

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestListConversations(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, []map[string]any{
			{"id": 1, "token": "abc123", "type": RoomTypeOneToOne, "displayName": "Bob", "participantType": ParticipantTypeUser},
			{"id": 2, "token": "def456", "type": RoomTypeGroup, "displayName": "Project", "participantType": ParticipantTypeOwner, "description": "Planning"},
		})
	})

	convs, err := client.ListConversations(context.Background())
	if err != nil {
		t.Fatalf("ListConversations failed: %v", err)
	}
	if last.Path != SpreedAPI+"/api/v4/room" {
		t.Errorf("path = %q", last.Path)
	}
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2", len(convs))
	}
	if convs[0].Token != "abc123" || !convs[0].IsOneToOne() {
		t.Errorf("first conversation decoded wrong: %+v", convs[0])
	}
	if convs[1].Description != "Planning" || !convs[1].IsModerator() {
		t.Errorf("second conversation decoded wrong: %+v", convs[1])
	}
}

func TestListConversationsPropagatesError(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusOK, http.StatusForbidden, "Nope")
	})
	if _, err := client.ListConversations(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGetConversationEscapesToken(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"token": "abc123", "displayName": "Project", "type": RoomTypeGroup})
	})

	conv, err := client.GetConversation(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("GetConversation failed: %v", err)
	}
	if conv.DisplayName != "Project" {
		t.Errorf("displayName = %q", conv.DisplayName)
	}
	if !strings.HasSuffix(last.Path, "/room/abc123") {
		t.Errorf("path = %q", last.Path)
	}
}

func TestListParticipants(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, []map[string]any{
			{"actorType": ActorUsers, "actorId": "alice", "displayName": "Alice", "participantType": ParticipantTypeOwner},
			{"actorType": ActorUsers, "actorId": "bob", "displayName": "Bob", "participantType": ParticipantTypeUser},
			{"actorType": ActorGuests, "actorId": "guest1", "displayName": "Guest", "participantType": ParticipantTypeGuest},
		})
	})

	parts, err := client.ListParticipants(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("ListParticipants failed: %v", err)
	}
	if !strings.HasSuffix(last.Path, "/room/abc123/participants") {
		t.Errorf("path = %q", last.Path)
	}
	if len(parts) != 3 {
		t.Fatalf("got %d participants, want 3", len(parts))
	}
	if !parts[0].IsModerator() {
		t.Error("owner should be a moderator")
	}
	if parts[1].IsModerator() {
		t.Error("plain user should not be a moderator")
	}
}

func TestCapabilitiesUnwrapsNestedPayload(t *testing.T) {
	// The capability payload is nested two levels deeper than a normal OCS
	// response, which is easy to get wrong.
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{
			"capabilities": map[string]any{
				"spreed": map[string]any{
					"features": []string{CapBotsV1, CapReactions, CapEditMessages},
					"config":   map[string]any{"chat": map[string]any{"max-length": 32000}},
				},
			},
		})
	})

	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities failed: %v", err)
	}
	if last.Path != "/ocs/v2.php/cloud/capabilities" {
		t.Errorf("path = %q", last.Path)
	}
	if !caps.Has(CapBotsV1) || !caps.Has(CapReactions) {
		t.Errorf("expected features to be present, got %v", caps.Features)
	}
	if caps.Has(CapDeleteMessages) {
		t.Error("Has should not report an absent feature")
	}
	if caps.Config == nil {
		t.Error("config was not decoded")
	}
}

func TestGetUserDetails(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{
			"id": "bob", "displayname": "Bob Example", "email": "bob@example.com", "enabled": true,
		})
	})

	details, err := client.GetUserDetails(context.Background(), "bob")
	if err != nil {
		t.Fatalf("GetUserDetails failed: %v", err)
	}
	if !strings.HasSuffix(last.Path, "/cloud/users/bob") {
		t.Errorf("path = %q", last.Path)
	}
	if details.DisplayName != "Bob Example" || details.Email != "bob@example.com" {
		t.Errorf("decoded %+v", details)
	}
}

func TestGetUserDetailsPropagatesForbidden(t *testing.T) {
	// Reading another user's record needs privileges many logins lack; the
	// caller falls back to message metadata, so the error must surface.
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusOK, http.StatusForbidden, "Not allowed")
	})
	if _, err := client.GetUserDetails(context.Background(), "carol"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAvatarURLs(t *testing.T) {
	client := NewClient("https://cloud.example.com", "alice", "pw")

	if got, want := client.AvatarURL("bob", 512), "https://cloud.example.com/avatar/bob/512"; got != want {
		t.Errorf("AvatarURL = %q, want %q", got, want)
	}
	// User IDs may contain characters that need escaping in a path segment.
	if got := client.AvatarURL("bob smith", 96); strings.Contains(got, " ") {
		t.Errorf("AvatarURL did not escape the user ID: %q", got)
	}

	want := "https://cloud.example.com" + SpreedAPI + "/api/v1/room/abc123/avatar"
	if got := client.ConversationAvatarURL("abc123"); got != want {
		t.Errorf("ConversationAvatarURL = %q, want %q", got, want)
	}
}
