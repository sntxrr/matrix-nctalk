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
	"errors"
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

const testHost = "cloud.example.com"
const testServer = "https://" + testHost

// newConverterClient returns a client with no HTTP server behind it, for the
// conversion paths that never make a request.
func newConverterClient(t *testing.T) *NCTalkClient {
	t.Helper()
	return newTestClient(t, testServer, "alice", Config{})
}

func TestConvertToTalkPlainText(t *testing.T) {
	client := newConverterClient(t)
	portal := newTestPortal(testHost, "abc123")
	msg := newTestMatrixMessage(portal, &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    "  hello there  ",
	})

	out, err := client.convertToTalk(context.Background(), "abc123", msg)
	if err != nil {
		t.Fatalf("convertToTalk: %v", err)
	}
	if out.Text != "hello there" {
		t.Errorf("Text = %q, want %q", out.Text, "hello there")
	}
	if out.ReplyTo != 0 || out.ThreadID != 0 {
		t.Errorf("expected no relations, got replyTo=%d threadId=%d", out.ReplyTo, out.ThreadID)
	}
	if out.ReferenceID != referenceID(msg.Event.ID) {
		t.Errorf("ReferenceID = %q", out.ReferenceID)
	}
}

func TestConvertToTalkFormattedBody(t *testing.T) {
	client := newConverterClient(t)
	portal := newTestPortal(testHost, "abc123")

	tests := []struct {
		name string
		html string
		want string
	}{
		{"bold", "<strong>loud</strong>", "**loud**"},
		{"italic", "<em>quiet</em>", "_quiet_"},
		{"code block", "<pre><code>x := 1\n</code></pre>", "```\nx := 1\n```"},
		{"inline code", "an <code>ident</code>", "an `ident`"},
		{"link with text", `<a href="https://example.com">site</a>`, "[site](https://example.com)"},
		{"blockquote", "<blockquote>quoted</blockquote>", "> quoted"},
		{"list", "<ul><li>one</li><li>two</li></ul>", "* one\n* two"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := newTestMatrixMessage(portal, &event.MessageEventContent{
				MsgType:       event.MsgText,
				Body:          "plaintext fallback",
				Format:        event.FormatHTML,
				FormattedBody: tc.html,
			})
			out, err := client.convertToTalk(context.Background(), "abc123", msg)
			if err != nil {
				t.Fatalf("convertToTalk: %v", err)
			}
			if out.Text != tc.want {
				t.Errorf("Text = %q, want %q", out.Text, tc.want)
			}
		})
	}
}

// An HTML format the bridge was not given content for must fall back to the
// plaintext body rather than sending nothing.
func TestConvertToTalkEmptyFormattedBody(t *testing.T) {
	client := newConverterClient(t)
	msg := newTestMatrixMessage(newTestPortal(testHost, "abc123"), &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          "plain only",
		Format:        event.FormatHTML,
		FormattedBody: "",
	})
	out, err := client.convertToTalk(context.Background(), "abc123", msg)
	if err != nil {
		t.Fatalf("convertToTalk: %v", err)
	}
	if out.Text != "plain only" {
		t.Errorf("Text = %q", out.Text)
	}
}

// Talk has no emote type, so an emote has to arrive as something that still
// reads as an action rather than as a flat statement.
func TestConvertToTalkEmote(t *testing.T) {
	client := newConverterClient(t)
	msg := newTestMatrixMessage(newTestPortal(testHost, "abc123"), &event.MessageEventContent{
		MsgType: event.MsgEmote,
		Body:    "waves",
	})
	out, err := client.convertToTalk(context.Background(), "abc123", msg)
	if err != nil {
		t.Fatalf("convertToTalk: %v", err)
	}
	if out.Text != "*waves*" {
		t.Errorf("Text = %q, want %q", out.Text, "*waves*")
	}
}

func TestConvertToTalkNotice(t *testing.T) {
	client := newConverterClient(t)
	msg := newTestMatrixMessage(newTestPortal(testHost, "abc123"), &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    "build failed",
	})
	out, err := client.convertToTalk(context.Background(), "abc123", msg)
	if err != nil {
		t.Fatalf("convertToTalk: %v", err)
	}
	if out.Text != "build failed" {
		t.Errorf("Text = %q", out.Text)
	}
}

func TestConvertToTalkRejections(t *testing.T) {
	client := newConverterClient(t)
	portal := newTestPortal(testHost, "abc123")

	tests := []struct {
		name    string
		content *event.MessageEventContent
		want    error
	}{
		{
			name:    "image",
			content: &event.MessageEventContent{MsgType: event.MsgImage, Body: "cat.png"},
			want:    bridgev2.ErrUnsupportedMessageType,
		},
		{
			name:    "location",
			content: &event.MessageEventContent{MsgType: event.MsgLocation, Body: "here"},
			want:    bridgev2.ErrUnsupportedMessageType,
		},
		{
			name:    "whitespace only",
			content: &event.MessageEventContent{MsgType: event.MsgText, Body: "   \n\t "},
			want:    errEmptyMessage,
		},
		{
			name: "too long",
			content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    strings.Repeat("a", nctalk.MaxChatLength+1),
			},
			want: errMessageTooLong,
		},
		{
			name:    "no content",
			content: nil,
			want:    bridgev2.ErrUnexpectedParsedContentType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := newTestMatrixMessage(portal, tc.content)
			_, err := client.convertToTalk(context.Background(), "abc123", msg)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// Length is measured the way Talk measures it: in characters, so a message of
// multi-byte characters that fits must not be rejected for its byte count.
func TestConvertToTalkCountsCharactersNotBytes(t *testing.T) {
	client := newConverterClient(t)
	msg := newTestMatrixMessage(newTestPortal(testHost, "abc123"), &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    strings.Repeat("é", nctalk.MaxChatLength),
	})
	if _, err := client.convertToTalk(context.Background(), "abc123", msg); err != nil {
		t.Fatalf("convertToTalk: %v", err)
	}
}

func TestConvertToTalkRelations(t *testing.T) {
	client := newConverterClient(t)
	portal := newTestPortal(testHost, "abc123")

	tests := []struct {
		name       string
		replyTo    networkid.MessageID
		threadRoot networkid.MessageID
		wantReply  int64
		wantThread int64
	}{
		{
			name:      "reply in the same conversation",
			replyTo:   makeMessageID(testHost, "abc123", 42),
			wantReply: 42,
		},
		{
			name:       "thread root with no reply",
			threadRoot: makeMessageID(testHost, "abc123", 7),
			wantThread: 7,
		},
		{
			// Talk derives the thread from the parent and rejects being given both.
			name:       "reply takes precedence over thread root",
			replyTo:    makeMessageID(testHost, "abc123", 42),
			threadRoot: makeMessageID(testHost, "abc123", 7),
			wantReply:  42,
		},
		{
			name:    "reply to another conversation is dropped",
			replyTo: makeMessageID(testHost, "other", 42),
		},
		{
			name:    "reply to another server is dropped",
			replyTo: makeMessageID("other.example.com", "abc123", 42),
		},
		{
			name:    "unparseable reply target is dropped",
			replyTo: "nonsense",
		},
		{
			// Relayed messages have no Talk message ID, so nothing can point at them.
			name:    "reply to a relayed message is dropped",
			replyTo: makeRelayedMessageID(testHost, "abc123", "deadbeef"),
		},
		{
			name:       "unparseable thread root is dropped",
			threadRoot: "nonsense",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := newTestMatrixMessage(portal, &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    "hello",
			})
			if tc.replyTo != "" {
				msg.ReplyTo = newTestTargetMessage(tc.replyTo)
			}
			if tc.threadRoot != "" {
				msg.ThreadRoot = newTestTargetMessage(tc.threadRoot)
			}

			out, err := client.convertToTalk(context.Background(), "abc123", msg)
			if err != nil {
				t.Fatalf("convertToTalk: %v", err)
			}
			if out.ReplyTo != tc.wantReply {
				t.Errorf("ReplyTo = %d, want %d", out.ReplyTo, tc.wantReply)
			}
			if out.ThreadID != tc.wantThread {
				t.Errorf("ThreadID = %d, want %d", out.ThreadID, tc.wantThread)
			}
		})
	}
}

func TestConvertToTalkMentions(t *testing.T) {
	const ghostMXID = "@nctalk_bob:matrix.example.com"

	tests := []struct {
		name   string
		ghosts map[id.UserID]networkid.UserID
		html   string
		want   string
	}{
		{
			name:   "user ghost becomes a Talk mention",
			ghosts: map[id.UserID]networkid.UserID{ghostMXID: makeUserID(testHost, nctalk.ActorUsers, "bob")},
			html:   `hi <a href="https://matrix.to/#/` + ghostMXID + `">Bob</a>`,
			want:   `hi @"bob"`,
		},
		{
			name:   "guest ghost is prefixed",
			ghosts: map[id.UserID]networkid.UserID{ghostMXID: makeUserID(testHost, nctalk.ActorGuests, "hash1")},
			html:   `hi <a href="https://matrix.to/#/` + ghostMXID + `">Guest</a>`,
			want:   `hi @"guest/hash1"`,
		},
		{
			name:   "email ghost is prefixed",
			ghosts: map[id.UserID]networkid.UserID{ghostMXID: makeUserID(testHost, nctalk.ActorEmails, "b@example.com")},
			html:   `hi <a href="https://matrix.to/#/` + ghostMXID + `">Bob</a>`,
			want:   `hi @"email/b@example.com"`,
		},
		{
			name:   "federated ghost is prefixed",
			ghosts: map[id.UserID]networkid.UserID{ghostMXID: makeUserID(testHost, nctalk.ActorFederatedUsers, "bob@other.example")},
			html:   `hi <a href="https://matrix.to/#/` + ghostMXID + `">Bob</a>`,
			want:   `hi @"federated_user/bob@other.example"`,
		},
		{
			// A bot has no mention form, so the pill must degrade to a plain name
			// rather than to a mention Talk would not resolve.
			name:   "bot ghost falls back to the display name",
			ghosts: map[id.UserID]networkid.UserID{ghostMXID: makeUserID(testHost, nctalk.ActorBots, "bot-1")},
			html:   `hi <a href="https://matrix.to/#/` + ghostMXID + `">Botty</a>`,
			want:   "hi Botty",
		},
		{
			name:   "a ghost on another server falls back to the display name",
			ghosts: map[id.UserID]networkid.UserID{ghostMXID: makeUserID("other.example.com", nctalk.ActorUsers, "bob")},
			html:   `hi <a href="https://matrix.to/#/` + ghostMXID + `">Bob</a>`,
			want:   "hi Bob",
		},
		{
			// A quote would close the mention early and leak the rest as text.
			name:   "an actor ID containing a quote falls back",
			ghosts: map[id.UserID]networkid.UserID{ghostMXID: makeUserID(testHost, nctalk.ActorUsers, `bo"b`)},
			html:   `hi <a href="https://matrix.to/#/` + ghostMXID + `">Bob</a>`,
			want:   "hi Bob",
		},
		{
			name:   "a real Matrix user is not a ghost",
			ghosts: map[id.UserID]networkid.UserID{},
			html:   `hi <a href="https://matrix.to/#/@carol:matrix.example.com">Carol</a>`,
			want:   "hi Carol",
		},
		{
			name:   "a malformed ghost ID falls back",
			ghosts: map[id.UserID]networkid.UserID{ghostMXID: "garbage"},
			html:   `hi <a href="https://matrix.to/#/` + ghostMXID + `">Bob</a>`,
			want:   "hi Bob",
		},
		{
			// Room and event links share the pill code path and must be untouched.
			name:   "a room alias link is left alone",
			ghosts: map[id.UserID]networkid.UserID{},
			html:   `see <a href="https://matrix.to/#/%23room:matrix.example.com">#room:matrix.example.com</a>`,
			want:   "see #room:matrix.example.com",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newConverterClient(t)
			client.mxidParser = &fakeGhostParser{ghosts: tc.ghosts}

			msg := newTestMatrixMessage(newTestPortal(testHost, "abc123"), &event.MessageEventContent{
				MsgType:       event.MsgText,
				Body:          "fallback",
				Format:        event.FormatHTML,
				FormattedBody: tc.html,
			})
			out, err := client.convertToTalk(context.Background(), "abc123", msg)
			if err != nil {
				t.Fatalf("convertToTalk: %v", err)
			}
			if out.Text != tc.want {
				t.Errorf("Text = %q, want %q", out.Text, tc.want)
			}
		})
	}
}

// Without a bridge or a stub there is nothing to resolve pills against, and the
// converter must degrade rather than panic.
func TestConvertToTalkMentionsWithoutResolver(t *testing.T) {
	client := newConverterClient(t)
	if client.ghosts() != nil {
		t.Fatal("expected no ghost resolver on a bridgeless test client")
	}

	msg := newTestMatrixMessage(newTestPortal(testHost, "abc123"), &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          "fallback",
		Format:        event.FormatHTML,
		FormattedBody: `hi <a href="https://matrix.to/#/@nctalk_bob:matrix.example.com">Bob</a>`,
	})
	out, err := client.convertToTalk(context.Background(), "abc123", msg)
	if err != nil {
		t.Fatalf("convertToTalk: %v", err)
	}
	if out.Text != "hi Bob" {
		t.Errorf("Text = %q, want %q", out.Text, "hi Bob")
	}
}

// The reference is what Talk stores to identify the message, so a resend of the
// same Matrix event has to produce the same one, and different events must not
// collide.
func TestReferenceID(t *testing.T) {
	first := referenceID("$event1:matrix.example.com")
	if first != referenceID("$event1:matrix.example.com") {
		t.Error("the same event ID should produce the same reference")
	}
	if first == referenceID("$event2:matrix.example.com") {
		t.Error("different event IDs should not collide")
	}
	// Talk truncates at 64 characters, so anything longer would be stored
	// differently to what the bridge recorded.
	if len(first) != 64 {
		t.Errorf("len = %d, want 64", len(first))
	}
}
