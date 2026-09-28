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
	"slices"
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

// fakeMentionTarget stands in for the bridge's login lookup and ghost MXID
// formatting.
type fakeMentionTarget struct {
	logins map[networkid.UserLoginID]id.UserID
}

func (f *fakeMentionTarget) LoginMXID(_ context.Context, loginID networkid.UserLoginID) (id.UserID, bool) {
	mxid, ok := f.logins[loginID]
	return mxid, ok
}

func (f *fakeMentionTarget) GhostMXID(userID networkid.UserID) id.UserID {
	_, actorType, actorID, _ := parseUserID(userID)
	return id.UserID("@nctalk_" + actorType + "_" + actorID + ":matrix.example.com")
}

func newMentionTestClient(t *testing.T) *NCTalkClient {
	t.Helper()
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	client.UserLogin.UserMXID = "@alice:matrix.example.com"
	client.mentions = &fakeMentionTarget{logins: map[networkid.UserLoginID]id.UserID{
		makeUserLoginID(client.host(), "carol"): "@carol:matrix.example.com",
	}}
	return client
}

func convertForTest(t *testing.T, client *NCTalkClient, msg *talkMessage) *event.MessageEventContent {
	t.Helper()
	converted, err := client.convertMessage(context.Background(), nil, nil, msg)
	if err != nil {
		t.Fatalf("convertMessage failed: %v", err)
	}
	return converted.Parts[0].Content
}

func TestConvertMessageMentionsBecomePills(t *testing.T) {
	cases := []struct {
		name  string
		param nctalk.MessageParam
		want  id.UserID
	}{{
		name:  "another Talk user is their ghost",
		param: nctalk.MessageParam{Type: "user", ID: "bob", Name: "Bob Example"},
		want:  "@nctalk_users_bob:matrix.example.com",
	}, {
		name:  "a user logged into the bridge is their real Matrix account",
		param: nctalk.MessageParam{Type: "user", ID: "carol", Name: "Carol"},
		want:  "@carol:matrix.example.com",
	}, {
		name:  "the login's own user is its Matrix account",
		param: nctalk.MessageParam{Type: "user", ID: "alice", Name: "Alice"},
		want:  "@alice:matrix.example.com",
	}, {
		name:  "a guest is their ghost",
		param: nctalk.MessageParam{Type: "guest", ID: "guest/abc123", Name: "Visitor"},
		want:  "@nctalk_guests_abc123:matrix.example.com",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newMentionTestClient(t)
			content := convertForTest(t, client, &talkMessage{
				Token: "abc123", MessageID: 1, SystemType: "message",
				Text:       "hi {mention-x1}!",
				Parameters: map[string]nctalk.MessageParam{"mention-x1": tc.param},
			})

			if content.Body != "hi "+tc.param.Name+"!" {
				t.Errorf("body = %q", content.Body)
			}
			wantPill := `<a href="https://matrix.to/#/` + string(tc.want) + `">` + tc.param.Name + `</a>`
			if content.Format != event.FormatHTML || !strings.Contains(content.FormattedBody, wantPill) {
				t.Errorf("formatted body = %q, want it to contain %q", content.FormattedBody, wantPill)
			}
			if content.Mentions == nil || !slices.Equal(content.Mentions.UserIDs, []id.UserID{tc.want}) {
				t.Errorf("m.mentions = %+v, want %s", content.Mentions, tc.want)
			}
		})
	}
}

func TestConvertMessageUnmappableMentionStaysText(t *testing.T) {
	client := newMentionTestClient(t)
	content := convertForTest(t, client, &talkMessage{
		Token: "abc123", MessageID: 1, SystemType: "message",
		Text: "hi {mention-group1} and {mention-federated-user1}",
		Parameters: map[string]nctalk.MessageParam{
			"mention-group1":          {Type: "user-group", ID: "admins", Name: "Admins"},
			"mention-federated-user1": {Type: "federated_user", ID: "dave", Server: "other.example.org", Name: "Dave"},
		},
	})

	if content.Body != "hi Admins and Dave" {
		t.Errorf("body = %q", content.Body)
	}
	if content.FormattedBody != "" {
		t.Errorf("an unmappable mention should not produce a pill: %q", content.FormattedBody)
	}
	if content.Mentions != nil && len(content.Mentions.UserIDs) > 0 {
		t.Errorf("m.mentions = %+v, want none", content.Mentions)
	}
}

func TestConvertMessageWithoutBridgeKeepsMentionsAsText(t *testing.T) {
	client := newTestClient(t, "https://cloud.example.com", "alice", Config{})
	content := convertForTest(t, client, &talkMessage{
		Token: "abc123", MessageID: 1, SystemType: "message",
		Text:       "hi {mention-user1}",
		Parameters: map[string]nctalk.MessageParam{"mention-user1": {Type: "user", ID: "bob", Name: "Bob"}},
	})
	if content.Body != "hi Bob" || content.FormattedBody != "" {
		t.Errorf("content = %+v", content)
	}
}

func TestConvertMessageSystemActorsAreNotMentions(t *testing.T) {
	client := newMentionTestClient(t)
	content := convertForTest(t, client, &talkMessage{
		Token: "abc123", MessageID: 1, SystemType: "user_added",
		Text: "{actor} added {user}",
		Parameters: map[string]nctalk.MessageParam{
			"actor": {Type: "user", ID: "bob", Name: "Bob"},
			"user":  {Type: "user", ID: "carol", Name: "Carol"},
		},
	})
	if content.Body != "Bob added Carol" {
		t.Errorf("body = %q", content.Body)
	}
	if content.FormattedBody != "" || (content.Mentions != nil && len(content.Mentions.UserIDs) > 0) {
		t.Errorf("a system message must not ping anyone: %+v", content)
	}
}

func TestConvertMessageAllMentionPingsRoom(t *testing.T) {
	client := newMentionTestClient(t)
	content := convertForTest(t, client, &talkMessage{
		Token: "abc123", MessageID: 1, SystemType: "message",
		Text:       "{mention-call1} standup",
		Parameters: map[string]nctalk.MessageParam{"mention-call1": {Type: "call", ID: "abc123", Name: "Team"}},
	})
	if content.Mentions == nil || !content.Mentions.Room {
		t.Errorf("m.mentions = %+v, want room", content.Mentions)
	}
	if content.Body != "Team standup" {
		t.Errorf("body = %q", content.Body)
	}
}

func TestConvertMessageMarkdownMentionBecomesPill(t *testing.T) {
	client := newMentionTestClient(t)
	content := convertForTest(t, client, &talkMessage{
		Token: "abc123", MessageID: 1, SystemType: "message", IsMarkdown: true,
		Text: "**look** {mention-user1} and {mention-user2}",
		Parameters: map[string]nctalk.MessageParam{
			"mention-user1": {Type: "user", ID: "bob", Name: "Bob"},
			"mention-user2": {Type: "user", ID: "carol", Name: "Carol"},
		},
	})

	for _, want := range []string{
		"<strong>look</strong>",
		`<a href="https://matrix.to/#/@nctalk_users_bob:matrix.example.com">Bob</a>`,
		`<a href="https://matrix.to/#/@carol:matrix.example.com">Carol</a>`,
	} {
		if !strings.Contains(content.FormattedBody, want) {
			t.Errorf("formatted body = %q, want it to contain %q", content.FormattedBody, want)
		}
	}
	if strings.Contains(content.FormattedBody, "nctalkmention") || strings.Contains(content.Body, "nctalkmention") {
		t.Errorf("a placeholder token leaked: %+v", content)
	}
	want := []id.UserID{"@nctalk_users_bob:matrix.example.com", "@carol:matrix.example.com"}
	if content.Mentions == nil || !slices.Equal(content.Mentions.UserIDs, want) {
		t.Errorf("m.mentions = %+v, want %v", content.Mentions, want)
	}
}

func TestConvertMessageMentionEscapesHTML(t *testing.T) {
	client := newMentionTestClient(t)
	content := convertForTest(t, client, &talkMessage{
		Token: "abc123", MessageID: 1, SystemType: "message",
		Text:       "<b>x</b> {mention-user1}",
		Parameters: map[string]nctalk.MessageParam{"mention-user1": {Type: "user", ID: "bob", Name: "Bob <3"}},
	})
	if strings.Contains(content.FormattedBody, "<b>") || !strings.Contains(content.FormattedBody, "&lt;b&gt;x&lt;/b&gt;") {
		t.Errorf("text was not escaped: %q", content.FormattedBody)
	}
	if !strings.Contains(content.FormattedBody, ">Bob &lt;3</a>") {
		t.Errorf("name was not escaped: %q", content.FormattedBody)
	}
	if content.Body != "<b>x</b> Bob <3" {
		t.Errorf("body = %q", content.Body)
	}
}
