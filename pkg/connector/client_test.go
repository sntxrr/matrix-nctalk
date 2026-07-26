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
	"context"
	"net/http"
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

func TestClientIsLoggedIn(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	if !client.IsLoggedIn() {
		t.Error("a login with an app password should be logged in")
	}

	client.meta().AppPassword = ""
	if client.IsLoggedIn() {
		t.Error("a login with no app password should not be logged in")
	}
}

func TestClientIsThisUser(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	host := client.host()

	if !client.IsThisUser(context.Background(), makeUserID(host, nctalk.ActorUsers, "alice")) {
		t.Error("should recognise its own account")
	}
	if client.IsThisUser(context.Background(), makeUserID(host, nctalk.ActorUsers, "bob")) {
		t.Error("should not claim another user")
	}
	// A guest whose actor ID happens to match must not be mistaken for the user.
	if client.IsThisUser(context.Background(), makeUserID(host, nctalk.ActorGuests, "alice")) {
		t.Error("should not match across actor types")
	}
	if client.IsThisUser(context.Background(), makeUserID("other.example.com", nctalk.ActorUsers, "alice")) {
		t.Error("should not match the same username on a different server")
	}
	if client.IsThisUser(context.Background(), "garbage") {
		t.Error("should not match a malformed user ID")
	}
}

func TestClientEventSender(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	host := client.host()

	self := client.eventSender(nctalk.ActorUsers, "alice")
	if !self.IsFromMe {
		t.Error("own messages should be marked as from me")
	}
	if self.SenderLogin != client.UserLogin.ID {
		t.Errorf("SenderLogin = %q, want %q", self.SenderLogin, client.UserLogin.ID)
	}
	if self.Sender != makeUserID(host, nctalk.ActorUsers, "alice") {
		t.Errorf("Sender = %q", self.Sender)
	}

	other := client.eventSender(nctalk.ActorUsers, "bob")
	if other.IsFromMe {
		t.Error("another user's message should not be marked as from me")
	}
	if other.SenderLogin != "" {
		t.Errorf("SenderLogin = %q, want empty for another user", other.SenderLogin)
	}
}

func TestGetChatInfoGroupConversation(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/participants"):
			writeOCS(t, w, []map[string]any{
				{"actorType": nctalk.ActorUsers, "actorId": "alice", "displayName": "Alice", "participantType": nctalk.ParticipantTypeOwner},
				{"actorType": nctalk.ActorUsers, "actorId": "bob", "displayName": "Bob", "participantType": nctalk.ParticipantTypeUser},
				// Actor types with no Matrix equivalent must be skipped rather
				// than producing broken ghosts.
				{"actorType": "circles", "actorId": "circle1", "displayName": "A Circle", "participantType": nctalk.ParticipantTypeUser},
			})
		default:
			writeOCS(t, w, map[string]any{
				"token": "abc123", "type": nctalk.RoomTypeGroup,
				"displayName": "Project", "description": "Planning things",
				"participantType": nctalk.ParticipantTypeOwner,
			})
		}
	})
	client := newTestClient(t, url, "alice", Config{})
	portal := newTestPortal(client.host(), "abc123")

	info, err := client.GetChatInfo(context.Background(), portal)
	if err != nil {
		t.Fatalf("GetChatInfo failed: %v", err)
	}
	if info.Name == nil || *info.Name != "Project" {
		t.Errorf("name = %v", info.Name)
	}
	if info.Topic == nil || *info.Topic != "Planning things" {
		t.Errorf("topic = %v", info.Topic)
	}
	if info.Type == nil || *info.Type != database.RoomTypeDefault {
		t.Errorf("room type = %v, want default", info.Type)
	}
	if len(info.Members.MemberMap) != 2 {
		t.Errorf("got %d members, want 2 (the circle should be skipped)", len(info.Members.MemberMap))
	}

	owner := info.Members.MemberMap[makeUserID(client.host(), nctalk.ActorUsers, "alice")]
	if owner.PowerLevel == nil || *owner.PowerLevel != 50 {
		t.Errorf("owner power level = %v, want 50", owner.PowerLevel)
	}
	member := info.Members.MemberMap[makeUserID(client.host(), nctalk.ActorUsers, "bob")]
	if member.PowerLevel != nil {
		t.Errorf("plain member power level = %v, want unset", member.PowerLevel)
	}
	if member.UserInfo == nil || member.UserInfo.Name == nil || *member.UserInfo.Name != "Bob" {
		t.Error("member display name was not carried through")
	}
}

func TestGetChatInfoMarksOneToOneAsDM(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/participants") {
			writeOCS(t, w, []map[string]any{
				{"actorType": nctalk.ActorUsers, "actorId": "alice", "participantType": nctalk.ParticipantTypeUser},
				{"actorType": nctalk.ActorUsers, "actorId": "bob", "participantType": nctalk.ParticipantTypeUser},
			})
			return
		}
		writeOCS(t, w, map[string]any{"token": "dm1", "type": nctalk.RoomTypeOneToOne, "displayName": "Bob"})
	})
	client := newTestClient(t, url, "alice", Config{})

	info, err := client.GetChatInfo(context.Background(), newTestPortal(client.host(), "dm1"))
	if err != nil {
		t.Fatalf("GetChatInfo failed: %v", err)
	}
	if info.Type == nil || *info.Type != database.RoomTypeDM {
		t.Errorf("room type = %v, want DM", info.Type)
	}
}

func TestGetChatInfoRejectsForeignPortal(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	portal := newTestPortal("other.example.com", "abc123")

	if _, err := client.GetChatInfo(context.Background(), portal); err == nil {
		t.Fatal("expected an error for a portal from a different server")
	}
}

func TestGetChatInfoRejectsMalformedPortalID(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	portal := &bridgev2.Portal{Portal: &database.Portal{
		PortalKey: networkid.PortalKey{ID: "garbage-no-separator"},
		Metadata:  &PortalMetadata{},
	}}

	if _, err := client.GetChatInfo(context.Background(), portal); err == nil {
		t.Fatal("expected an error for a malformed portal ID")
	}
}

func TestGetChatInfoPropagatesError(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusNotFound, "Conversation not found")
	})
	client := newTestClient(t, url, "alice", Config{})

	if _, err := client.GetChatInfo(context.Background(), newTestPortal(client.host(), "gone")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGetUserInfo(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": "bob", "displayname": "Bob Example"})
	})
	client := newTestClient(t, url, "alice", Config{})
	ghost := newTestGhost(client.host(), nctalk.ActorUsers, "bob")

	info, err := client.GetUserInfo(context.Background(), ghost)
	if err != nil {
		t.Fatalf("GetUserInfo failed: %v", err)
	}
	if info.Name == nil || *info.Name != "Bob Example" {
		t.Errorf("name = %v", info.Name)
	}
	if info.Avatar == nil {
		t.Error("expected an avatar for a real user")
	}
	if info.IsBot == nil || *info.IsBot {
		t.Error("a user should not be marked as a bot")
	}
	if len(info.Identifiers) == 0 {
		t.Error("expected an identifier")
	}
}

// Reading another user's record needs privileges many logins lack. That must
// degrade to no name rather than failing the whole ghost.
func TestGetUserInfoToleratesForbiddenProfile(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusForbidden, "Not allowed")
	})
	client := newTestClient(t, url, "alice", Config{})

	info, err := client.GetUserInfo(context.Background(), newTestGhost(client.host(), nctalk.ActorUsers, "bob"))
	if err != nil {
		t.Fatalf("GetUserInfo should tolerate a forbidden profile, got %v", err)
	}
	if info.Name != nil {
		t.Errorf("name = %v, want nil so message metadata supplies it", info.Name)
	}
	if info.Avatar == nil {
		t.Error("an avatar should still be offered")
	}
}

func TestGetUserInfoForNonUserActors(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})

	// Guests and bots have no profile endpoint, so no lookup should happen.
	for _, actorType := range []string{nctalk.ActorGuests, nctalk.ActorBots} {
		info, err := client.GetUserInfo(context.Background(), newTestGhost(client.host(), actorType, "someone"))
		if err != nil {
			t.Fatalf("GetUserInfo for %s failed: %v", actorType, err)
		}
		if info.Avatar != nil {
			t.Errorf("%s should not get an avatar", actorType)
		}
		if info.Name != nil {
			t.Errorf("%s should not get a looked-up name", actorType)
		}
	}

	botInfo, _ := client.GetUserInfo(context.Background(), newTestGhost(client.host(), nctalk.ActorBots, "bot-1"))
	if botInfo.IsBot == nil || !*botInfo.IsBot {
		t.Error("a bot actor should be marked as a bot")
	}
}

func TestGetUserInfoRejectsForeignGhost(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	ghost := newTestGhost("other.example.com", nctalk.ActorUsers, "bob")

	if _, err := client.GetUserInfo(context.Background(), ghost); err == nil {
		t.Fatal("expected an error for a ghost from a different server")
	}
}

func TestGetCapabilitiesReflectsServerFeatures(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	portal := newTestPortal(client.host(), "abc123")

	client.caps = &nctalk.Capabilities{Features: []string{
		nctalk.CapReactions, nctalk.CapEditMessages, nctalk.CapDeleteMessages,
		nctalk.CapChatReadStatus, nctalk.CapThreads,
	}}
	feats := client.GetCapabilities(context.Background(), portal)

	if feats.Reaction != event.CapLevelFullySupported {
		t.Error("reactions should be advertised when the server supports them")
	}
	if feats.Edit != event.CapLevelFullySupported || feats.EditMaxAge == nil {
		t.Error("edits should be advertised with Talk's 24 hour window")
	}
	if feats.Delete != event.CapLevelFullySupported || feats.DeleteMaxAge == nil {
		t.Error("deletes should be advertised with Talk's 6 hour window")
	}
	if !feats.ReadReceipts {
		t.Error("read receipts should be advertised")
	}
	if feats.Thread != event.CapLevelFullySupported {
		t.Error("threads should be advertised")
	}
	if feats.MaxTextLength != nctalk.MaxChatLength {
		t.Errorf("max text length = %d, want %d", feats.MaxTextLength, nctalk.MaxChatLength)
	}
}

func TestGetCapabilitiesOmitsUnsupportedFeatures(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	client.caps = &nctalk.Capabilities{Features: []string{nctalk.CapBotsV1}}

	feats := client.GetCapabilities(context.Background(), newTestPortal(client.host(), "abc123"))
	if feats.Reaction == event.CapLevelFullySupported {
		t.Error("reactions should not be advertised when unsupported")
	}
	if feats.Edit == event.CapLevelFullySupported || feats.EditMaxAge != nil {
		t.Error("edits should not be advertised when unsupported")
	}
	if feats.Delete == event.CapLevelFullySupported {
		t.Error("deletes should not be advertised when unsupported")
	}
	if feats.ReadReceipts {
		t.Error("read receipts should not be advertised when unsupported")
	}
}

// A portal opened before the first successful Connect must still advertise
// sanely, using the capability list cached at login.
func TestGetCapabilitiesFallsBackToStoredFeatures(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	client.caps = nil
	client.meta().Features = []string{nctalk.CapReactions}

	feats := client.GetCapabilities(context.Background(), newTestPortal(client.host(), "abc123"))
	if feats.Reaction != event.CapLevelFullySupported {
		t.Error("should fall back to the capability list stored at login")
	}
}

func TestDownloadUsesCredentials(t *testing.T) {
	var gotUser, gotPass string
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = w.Write([]byte("image-bytes"))
	})
	client := newTestClient(t, url, "alice", Config{})

	data, err := client.download(context.Background(), url+"/avatar/bob/512")
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if string(data) != "image-bytes" {
		t.Errorf("data = %q", data)
	}
	if gotUser != "alice" || gotPass != "app-password" {
		t.Errorf("credentials not sent: user=%q", gotUser)
	}
}

func TestDownloadRejectsErrorStatus(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	client := newTestClient(t, url, "alice", Config{})

	if _, err := client.download(context.Background(), url+"/missing"); err == nil {
		t.Fatal("expected an error for a 404")
	}
}

func TestLogoutRemoteRevokesAppPassword(t *testing.T) {
	var method, path string
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		writeOCS(t, w, nil)
	})
	client := newTestClient(t, url, "alice", Config{})

	client.LogoutRemote(context.Background())
	if method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", method)
	}
	if path != "/ocs/v2.php/core/apppassword" {
		t.Errorf("path = %q", path)
	}
}

func TestLogoutRemoteToleratesFailure(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := newTestClient(t, url, "alice", Config{})
	// Logging out of the bridge must succeed even if Nextcloud is unreachable.
	client.LogoutRemote(context.Background())
}

func TestDisconnectIsSafe(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	client.Disconnect()
}

func TestGetChatInfoFetchesCustomAvatar(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/avatar"):
			_, _ = w.Write([]byte("conversation-avatar-bytes"))
		case strings.HasSuffix(r.URL.Path, "/participants"):
			writeOCS(t, w, []map[string]any{
				{"actorType": nctalk.ActorUsers, "actorId": "alice", "participantType": nctalk.ParticipantTypeOwner},
			})
		default:
			writeOCS(t, w, map[string]any{
				"token": "abc123", "type": nctalk.RoomTypeGroup, "displayName": "Project",
				"isCustomAvatar": true, "avatarVersion": "v7",
			})
		}
	})
	client := newTestClient(t, url, "alice", Config{})

	info, err := client.GetChatInfo(context.Background(), newTestPortal(client.host(), "abc123"))
	if err != nil {
		t.Fatalf("GetChatInfo failed: %v", err)
	}
	if info.Avatar == nil {
		t.Fatal("expected an avatar for a conversation with a custom picture")
	}
	// The avatar version keys the cache, so a changed picture re-uploads and an
	// unchanged one does not.
	if !strings.Contains(string(info.Avatar.ID), "v7") {
		t.Errorf("avatar ID = %q, should include the avatar version", info.Avatar.ID)
	}

	data, err := info.Avatar.Get(context.Background())
	if err != nil {
		t.Fatalf("avatar fetch failed: %v", err)
	}
	if string(data) != "conversation-avatar-bytes" {
		t.Errorf("avatar data = %q", data)
	}
}

func TestGetChatInfoSkipsAvatarWhenNotCustom(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/participants") {
			writeOCS(t, w, []map[string]any{})
			return
		}
		writeOCS(t, w, map[string]any{"token": "abc123", "type": nctalk.RoomTypeGroup, "displayName": "Project"})
	})
	client := newTestClient(t, url, "alice", Config{})

	info, err := client.GetChatInfo(context.Background(), newTestPortal(client.host(), "abc123"))
	if err != nil {
		t.Fatalf("GetChatInfo failed: %v", err)
	}
	// Talk generates a default picture, so uploading it would just add noise.
	if info.Avatar != nil {
		t.Error("a conversation without a custom picture should not carry an avatar")
	}
}

func TestGetUserInfoAvatarFetch(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/avatar/") {
			_, _ = w.Write([]byte("user-avatar-bytes"))
			return
		}
		writeOCS(t, w, map[string]any{"id": "bob", "displayname": "Bob"})
	})
	client := newTestClient(t, url, "alice", Config{})

	info, err := client.GetUserInfo(context.Background(), newTestGhost(client.host(), nctalk.ActorUsers, "bob"))
	if err != nil {
		t.Fatalf("GetUserInfo failed: %v", err)
	}
	data, err := info.Avatar.Get(context.Background())
	if err != nil {
		t.Fatalf("avatar fetch failed: %v", err)
	}
	if string(data) != "user-avatar-bytes" {
		t.Errorf("avatar data = %q", data)
	}
}
