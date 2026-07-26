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

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

const testBotName = "Matrix Bridge"

func botConfig() Config {
	return Config{AutoEnableBot: true, BotName: testBotName, BotSecret: testBotSecret}
}

// botListServer answers the per-conversation bot list and records enable calls.
func botListServer(t *testing.T, bots []map[string]any) (string, *int) {
	t.Helper()
	enables := 0
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/bot/") {
			enables++
			writeOCS(t, w, map[string]any{"id": 7, "name": testBotName})
			return
		}
		writeOCS(t, w, bots)
	})
	return url, &enables
}

func TestEnsureBotEnabledEnablesForModerator(t *testing.T) {
	url, enables := botListServer(t, []map[string]any{
		{"id": 3, "name": "Some Other Bot", "state": nctalk.BotStateEnabled},
		{"id": 7, "name": testBotName, "state": nctalk.BotStateEnabled},
	})
	client := newTestClient(t, url, "alice", botConfig())
	portal := newTestPortal(client.host(), "abc123")
	conv := &nctalk.Conversation{Token: "abc123", Type: nctalk.RoomTypeGroup, ParticipantType: nctalk.ParticipantTypeOwner}

	client.ensureBotEnabled(context.Background(), portal, conv)

	meta := portal.Metadata.(*PortalMetadata)
	if !meta.BotEnabled {
		t.Error("bot should be recorded as enabled")
	}
	if meta.BotEnableFailed {
		t.Error("bot enable should not be recorded as failed")
	}
	if *enables != 1 {
		t.Errorf("made %d enable calls, want 1", *enables)
	}
}

// Without moderator rights Talk rejects the call, so the bridge should not try.
func TestEnsureBotEnabledSkipsNonModerator(t *testing.T) {
	url, enables := botListServer(t, nil)
	client := newTestClient(t, url, "alice", botConfig())
	portal := newTestPortal(client.host(), "abc123")
	conv := &nctalk.Conversation{Token: "abc123", Type: nctalk.RoomTypeGroup, ParticipantType: nctalk.ParticipantTypeUser}

	client.ensureBotEnabled(context.Background(), portal, conv)

	meta := portal.Metadata.(*PortalMetadata)
	if meta.BotEnabled {
		t.Error("bot should not be marked enabled")
	}
	if !meta.BotEnableFailed {
		t.Error("the attempt should be recorded so it is not retried on every sync")
	}
	if *enables != 0 {
		t.Errorf("made %d enable calls, want 0", *enables)
	}
}

// Talk's BotController rejects these conversation types outright.
func TestEnsureBotEnabledSkipsUnbridgeableConversations(t *testing.T) {
	for _, roomType := range []int{nctalk.RoomTypeChangelog, nctalk.RoomTypeOneToOneFormer} {
		url, enables := botListServer(t, nil)
		client := newTestClient(t, url, "alice", botConfig())
		portal := newTestPortal(client.host(), "abc123")
		conv := &nctalk.Conversation{Token: "abc123", Type: roomType, ParticipantType: nctalk.ParticipantTypeOwner}

		client.ensureBotEnabled(context.Background(), portal, conv)

		if portal.Metadata.(*PortalMetadata).BotEnabled {
			t.Errorf("room type %d should not be enabled", roomType)
		}
		if *enables != 0 {
			t.Errorf("room type %d: made %d enable calls, want 0", roomType, *enables)
		}
	}
}

// A bot installed with --no-setup cannot be enabled by a moderator, and the
// resulting 400 is confusing, so the bridge detects it up front.
func TestEnsureBotEnabledDetectsNoSetupState(t *testing.T) {
	url, enables := botListServer(t, []map[string]any{
		{"id": 7, "name": testBotName, "state": nctalk.BotStateNoSetup},
	})
	client := newTestClient(t, url, "alice", botConfig())
	portal := newTestPortal(client.host(), "abc123")
	conv := &nctalk.Conversation{Token: "abc123", Type: nctalk.RoomTypeGroup, ParticipantType: nctalk.ParticipantTypeOwner}

	client.ensureBotEnabled(context.Background(), portal, conv)

	meta := portal.Metadata.(*PortalMetadata)
	if meta.BotEnabled {
		t.Error("a no-setup bot cannot be enabled by a moderator")
	}
	if !meta.BotEnableFailed {
		t.Error("the failure should be recorded")
	}
	if *enables != 0 {
		t.Errorf("made %d enable calls, want 0", *enables)
	}
}

// The most likely misconfiguration is bot_name not matching the installed name.
func TestEnsureBotEnabledHandlesMissingBot(t *testing.T) {
	url, enables := botListServer(t, []map[string]any{
		{"id": 3, "name": "A Completely Different Bot", "state": nctalk.BotStateEnabled},
	})
	client := newTestClient(t, url, "alice", botConfig())
	portal := newTestPortal(client.host(), "abc123")
	conv := &nctalk.Conversation{Token: "abc123", Type: nctalk.RoomTypeGroup, ParticipantType: nctalk.ParticipantTypeOwner}

	client.ensureBotEnabled(context.Background(), portal, conv)

	meta := portal.Metadata.(*PortalMetadata)
	if meta.BotEnabled || !meta.BotEnableFailed {
		t.Error("a missing bot should be recorded as a failure")
	}
	if *enables != 0 {
		t.Errorf("made %d enable calls, want 0", *enables)
	}
}

func TestEnsureBotEnabledRecordsEnableFailure(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeOCSError(w, http.StatusBadRequest, "bot")
			return
		}
		writeOCS(t, w, []map[string]any{{"id": 7, "name": testBotName, "state": nctalk.BotStateEnabled}})
	})
	client := newTestClient(t, url, "alice", botConfig())
	portal := newTestPortal(client.host(), "abc123")
	conv := &nctalk.Conversation{Token: "abc123", Type: nctalk.RoomTypeGroup, ParticipantType: nctalk.ParticipantTypeOwner}

	client.ensureBotEnabled(context.Background(), portal, conv)

	meta := portal.Metadata.(*PortalMetadata)
	if meta.BotEnabled {
		t.Error("a rejected enable should not be recorded as enabled")
	}
	if !meta.BotEnableFailed {
		t.Error("the failure should be recorded")
	}
}

// Repeated portal syncs must not re-hit Nextcloud once the answer is known.
func TestEnsureBotEnabledIsIdempotent(t *testing.T) {
	url, enables := botListServer(t, []map[string]any{
		{"id": 7, "name": testBotName, "state": nctalk.BotStateEnabled},
	})
	client := newTestClient(t, url, "alice", botConfig())
	portal := newTestPortal(client.host(), "abc123")
	conv := &nctalk.Conversation{Token: "abc123", Type: nctalk.RoomTypeGroup, ParticipantType: nctalk.ParticipantTypeOwner}

	for range 3 {
		client.ensureBotEnabled(context.Background(), portal, conv)
	}
	if *enables != 1 {
		t.Errorf("made %d enable calls across 3 syncs, want 1", *enables)
	}

	// A recorded failure must also not be retried.
	failed := newTestPortal(client.host(), "def456")
	failed.Metadata.(*PortalMetadata).BotEnableFailed = true
	before := *enables
	client.ensureBotEnabled(context.Background(), failed, conv)
	if *enables != before {
		t.Error("a recorded failure should not be retried")
	}
}

func TestEnsureBotEnabledRespectsConfigToggle(t *testing.T) {
	url, enables := botListServer(t, []map[string]any{
		{"id": 7, "name": testBotName, "state": nctalk.BotStateEnabled},
	})
	cfg := botConfig()
	cfg.AutoEnableBot = false
	client := newTestClient(t, url, "alice", cfg)
	portal := newTestPortal(client.host(), "abc123")
	conv := &nctalk.Conversation{Token: "abc123", Type: nctalk.RoomTypeGroup, ParticipantType: nctalk.ParticipantTypeOwner}

	client.ensureBotEnabled(context.Background(), portal, conv)

	if *enables != 0 {
		t.Errorf("made %d enable calls with auto_enable_bot off, want 0", *enables)
	}
	if portal.Metadata.(*PortalMetadata).BotEnableFailed {
		t.Error("disabling the feature is not a failure and should not be recorded as one")
	}
}
