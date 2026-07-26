package connector

import (
	"testing"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

func TestRenderTalkMessage(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		params map[string]nctalk.MessageParam
		want   string
	}{{
		name: "no placeholders",
		text: "just a plain message",
		want: "just a plain message",
	}, {
		name:   "no parameters supplied",
		text:   "hello {mention-user1}",
		params: nil,
		want:   "hello {mention-user1}",
	}, {
		name: "user mention",
		text: "hello {mention-user1}",
		params: map[string]nctalk.MessageParam{
			"mention-user1": {Type: "user", ID: "bob", Name: "Bob Example"},
		},
		want: "hello Bob Example",
	}, {
		name: "multiple placeholders",
		text: "{actor} shared {file} with {mention-user1}",
		params: map[string]nctalk.MessageParam{
			"actor":         {Type: "user", ID: "alice", Name: "Alice"},
			"file":          {Type: "file", ID: "42", Name: "report.pdf"},
			"mention-user1": {Type: "user", ID: "bob", Name: "Bob"},
		},
		want: "Alice shared report.pdf with Bob",
	}, {
		// An unmatched placeholder must survive rather than vanish, so no part
		// of a message is silently lost.
		name: "unknown placeholder is preserved",
		text: "value is {unknown} here",
		params: map[string]nctalk.MessageParam{
			"other": {Type: "user", Name: "Alice"},
		},
		want: "value is {unknown} here",
	}, {
		name: "unclosed brace is preserved",
		text: "this { is not a placeholder",
		params: map[string]nctalk.MessageParam{
			"is": {Type: "user", Name: "Alice"},
		},
		want: "this { is not a placeholder",
	}, {
		name: "adjacent placeholders",
		text: "{a}{b}",
		params: map[string]nctalk.MessageParam{
			"a": {Type: "user", Name: "Alice"},
			"b": {Type: "user", Name: "Bob"},
		},
		want: "AliceBob",
	}, {
		name: "placeholder at start and end",
		text: "{a} and {b}",
		params: map[string]nctalk.MessageParam{
			"a": {Type: "user", Name: "Alice"},
			"b": {Type: "user", Name: "Bob"},
		},
		want: "Alice and Bob",
	}, {
		name: "falls back to ID when name is empty",
		text: "from {actor}",
		params: map[string]nctalk.MessageParam{
			"actor": {Type: "user", ID: "alice"},
		},
		want: "from alice",
	}, {
		name: "curly braces in substituted value are not re-expanded",
		text: "{a}",
		params: map[string]nctalk.MessageParam{
			"a": {Type: "user", Name: "{b}"},
			"b": {Type: "user", Name: "should not appear"},
		},
		want: "{b}",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderTalkMessage(tc.text, tc.params); got != tc.want {
				t.Errorf("renderTalkMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderParamFallbacks(t *testing.T) {
	tests := []struct {
		name  string
		param nctalk.MessageParam
		want  string
	}{
		{name: "file with name", param: nctalk.MessageParam{Type: nctalk.ParamTypeFile, Name: "a.pdf"}, want: "a.pdf"},
		{name: "file without name", param: nctalk.MessageParam{Type: nctalk.ParamTypeFile}, want: "file"},
		{name: "location without name", param: nctalk.MessageParam{Type: nctalk.ParamTypeGeoLocation}, want: "location"},
		{name: "unknown type with name", param: nctalk.MessageParam{Type: "talk-poll", Name: "Lunch?"}, want: "Lunch?"},
		{name: "unknown type with only ID", param: nctalk.MessageParam{Type: "talk-poll", ID: "7"}, want: "7"},
		{name: "empty param", param: nctalk.MessageParam{Type: "mystery"}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderParam(tc.param); got != tc.want {
				t.Errorf("renderParam() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTalkMessageIsSystem(t *testing.T) {
	if (&talkMessage{SystemType: "message"}).isSystem() {
		t.Error("a chat message should not be treated as a system message")
	}
	if (&talkMessage{SystemType: ""}).isSystem() {
		t.Error("an empty system type should not be treated as a system message")
	}
	if !(&talkMessage{SystemType: "call_started"}).isSystem() {
		t.Error("call_started should be treated as a system message")
	}
}

func TestBackendHost(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		// Talk sends the base URL with a trailing slash.
		{in: "https://cloud.example.com/", want: "cloud.example.com"},
		{in: "https://cloud.example.com", want: "cloud.example.com"},
		{in: "https://cloud.example.com:8443/", want: "cloud.example.com:8443"},
		{in: "https://example.com/nextcloud/", want: "example.com"},
		{in: "  https://cloud.example.com/  ", want: "cloud.example.com"},
		{in: "", wantErr: true},
		{in: "not a url", wantErr: true},
	}
	for _, tc := range tests {
		got, err := backendHost(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("backendHost(%q): expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("backendHost(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("backendHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
