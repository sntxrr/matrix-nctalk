package nctalk

import "testing"

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
