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
	"encoding/json"
	"testing"
)

func TestConversationIsOneToOne(t *testing.T) {
	tests := []struct {
		roomType int
		want     bool
	}{
		{RoomTypeOneToOne, true},
		// A former one-to-one is still a DM for room-type purposes, even though
		// the bot cannot be enabled in it.
		{RoomTypeOneToOneFormer, true},
		{RoomTypeGroup, false},
		{RoomTypePublic, false},
		{RoomTypeNoteToSelf, false},
		{RoomTypeChangelog, false},
	}
	for _, tc := range tests {
		if got := (&Conversation{Type: tc.roomType}).IsOneToOne(); got != tc.want {
			t.Errorf("IsOneToOne for type %d = %v, want %v", tc.roomType, got, tc.want)
		}
	}
}

// Moderator status gates whether the bridge attempts to enable its own bot, so
// a wrong answer here silently leaves conversations one-way.
func TestConversationIsModerator(t *testing.T) {
	tests := []struct {
		participantType int
		want            bool
	}{
		{ParticipantTypeOwner, true},
		{ParticipantTypeModerator, true},
		{ParticipantTypeGuestMod, true},
		{ParticipantTypeUser, false},
		{ParticipantTypeGuest, false},
		{ParticipantTypeUserSelf, false},
	}
	for _, tc := range tests {
		if got := (&Conversation{ParticipantType: tc.participantType}).IsModerator(); got != tc.want {
			t.Errorf("Conversation.IsModerator for type %d = %v, want %v", tc.participantType, got, tc.want)
		}
		if got := (&Participant{ParticipantType: tc.participantType}).IsModerator(); got != tc.want {
			t.Errorf("Participant.IsModerator for type %d = %v, want %v", tc.participantType, got, tc.want)
		}
	}
}

// Talk's BotController rejects these conversation types outright, so attempting
// to enable the bot in them would produce a confusing 400.
func TestConversationBridgeable(t *testing.T) {
	tests := []struct {
		roomType int
		want     bool
	}{
		{RoomTypeOneToOne, true},
		{RoomTypeGroup, true},
		{RoomTypePublic, true},
		{RoomTypeNoteToSelf, true},
		{RoomTypeChangelog, false},
		{RoomTypeOneToOneFormer, false},
	}
	for _, tc := range tests {
		if got := (&Conversation{Type: tc.roomType}).Bridgeable(); got != tc.want {
			t.Errorf("Bridgeable for type %d = %v, want %v", tc.roomType, got, tc.want)
		}
	}
}

func TestCapabilitiesHas(t *testing.T) {
	caps := &Capabilities{Features: []string{CapBotsV1, CapReactions, CapEditMessages}}

	for _, feature := range []string{CapBotsV1, CapReactions, CapEditMessages} {
		if !caps.Has(feature) {
			t.Errorf("Has(%q) = false, want true", feature)
		}
	}
	for _, feature := range []string{CapDeleteMessages, CapThreads, "nonexistent"} {
		if caps.Has(feature) {
			t.Errorf("Has(%q) = true, want false", feature)
		}
	}

	empty := &Capabilities{}
	if empty.Has(CapBotsV1) {
		t.Error("an empty capability set should report no features")
	}
}

func TestBotFeatureBitsMatchSpreed(t *testing.T) {
	// These mirror Bot::FEATURE_* in spreed; drift would silently install the
	// bot with the wrong capabilities.
	if BotFeatureWebhook != 1 || BotFeatureResponse != 2 || BotFeatureEvent != 4 || BotFeatureReaction != 8 {
		t.Error("bot feature bits no longer match spreed's Bot::FEATURE_* values")
	}
	want := BotFeatureWebhook | BotFeatureResponse | BotFeatureReaction
	if BotFeaturesBridge != want {
		t.Errorf("BotFeaturesBridge = %d, want %d", BotFeaturesBridge, want)
	}
	// The bridge must not claim FEATURE_EVENT: that is for in-server apps.
	if BotFeaturesBridge&BotFeatureEvent != 0 {
		t.Error("the bridge bot should not request the event feature")
	}
}

func TestBotStateConstantsMatchSpreed(t *testing.T) {
	if BotStateDisabled != 0 || BotStateEnabled != 1 || BotStateNoSetup != 2 || BotStateUnavailable != 3 {
		t.Error("bot state constants no longer match spreed's Bot::STATE_* values")
	}
}

// PHP encodes an empty associative array as `[]`, so Talk sends that for every
// message with no rich objects. Decoding straight into a map rejects those,
// which is the common case, not an edge case.
func TestMessageParamsAcceptsPHPEmptyArray(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    int
		wantErr bool
	}{
		{name: "empty array", json: `{"parameters":[]}`, want: 0},
		{name: "null", json: `{"parameters":null}`, want: 0},
		{name: "absent", json: `{}`, want: 0},
		{name: "empty object", json: `{"parameters":{}}`, want: 0},
		{
			name: "populated object",
			json: `{"parameters":{"actor":{"type":"user","id":"alice","name":"Alice"}}}`,
			want: 1,
		},
		// A non-empty array is not the quirk, it is a payload we do not understand.
		{name: "populated array", json: `{"parameters":["nope"]}`, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out struct {
				Parameters MessageParams `json:"parameters"`
			}
			err := json.Unmarshal([]byte(tc.json), &out)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(out.Parameters) != tc.want {
				t.Errorf("got %d parameters, want %d", len(out.Parameters), tc.want)
			}
		})
	}
}

// The same quirk applies to the chat API's own message payload.
func TestMessageDecodesEmptyParameters(t *testing.T) {
	var msg Message
	raw := `{"id":72,"actorId":"bob","message":"hello","messageParameters":[]}`
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.ID != 72 || len(msg.MessageParameters) != 0 {
		t.Errorf("msg = %+v", msg)
	}
}
