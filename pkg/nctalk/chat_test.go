package nctalk

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSendMessage(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{
			"id":        4711,
			"token":     "abc123",
			"actorType": ActorUsers,
			"actorId":   testUser,
			"message":   "hello",
			"timestamp": 1700000000,
		})
	})

	sent, err := client.SendMessage(context.Background(), "abc123", SendMessageRequest{
		Message:     "hello",
		ReferenceID: "ref-1",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if sent.ID != 4711 {
		t.Errorf("ID = %d, want 4711", sent.ID)
	}
	if sent.Timestamp != 1700000000 {
		t.Errorf("Timestamp = %d", sent.Timestamp)
	}

	if last.Method != http.MethodPost {
		t.Errorf("Method = %s, want POST", last.Method)
	}
	if want := SpreedAPI + "/api/v1/chat/abc123"; last.Path != want {
		t.Errorf("Path = %s, want %s", last.Path, want)
	}
	if !last.HasAuth || last.User != testUser || last.Pass != testPass {
		t.Errorf("expected the login's credentials, got %q/%q (auth=%v)", last.User, last.Pass, last.HasAuth)
	}
	if got := last.Header.Get("OCS-APIRequest"); got != "true" {
		t.Errorf("OCS-APIRequest = %q", got)
	}
	form, err := url.ParseQuery(last.Body)
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if form.Get("message") != "hello" {
		t.Errorf("message = %q", form.Get("message"))
	}
	if form.Get("referenceId") != "ref-1" {
		t.Errorf("referenceId = %q", form.Get("referenceId"))
	}
}

// A conversation token with characters that need escaping must not be able to
// reach outside its own path segment.
func TestSendMessageEscapesToken(t *testing.T) {
	var escaped string
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// The decoded path cannot show whether the token was escaped; only the
		// bytes actually sent can.
		escaped = r.URL.EscapedPath()
		writeOCS(t, w, map[string]any{"id": 1})
	})
	_, err := client.SendMessage(context.Background(), "a/../b", SendMessageRequest{Message: "x"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if want := SpreedAPI + "/api/v1/chat/a%2F..%2Fb"; escaped != want {
		t.Errorf("escaped path = %s, want %s", escaped, want)
	}
}

// Talk returns the message it created, but omits it when the sender cannot see
// it. The bridge needs the ID, so that has to be an error rather than a message
// with ID zero.
func TestSendMessageWithoutReturnedMessage(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ocs":{"meta":{"status":"ok","statuscode":201,"message":"OK"},"data":null}}`))
	})

	_, err := client.SendMessage(context.Background(), "abc123", SendMessageRequest{Message: "hello"})
	if !errors.Is(err, ErrMessageNotReturned) {
		t.Fatalf("err = %v, want ErrMessageNotReturned", err)
	}
}

// Talk does not echo the token on every response shape, and the caller already
// knows it, so it is filled in rather than left empty.
func TestSendMessageFillsToken(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": 7})
	})
	sent, err := client.SendMessage(context.Background(), "abc123", SendMessageRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if sent.Token != "abc123" {
		t.Errorf("Token = %q, want abc123", sent.Token)
	}
}

func TestSendMessageRequestForm(t *testing.T) {
	longRef := strings.Repeat("a", maxReferenceIDLength+10)

	tests := []struct {
		name string
		req  SendMessageRequest
		want url.Values
	}{
		{
			name: "text only",
			req:  SendMessageRequest{Message: "hello"},
			want: url.Values{"message": {"hello"}},
		},
		{
			name: "reply",
			req:  SendMessageRequest{Message: "hello", ReplyTo: 42},
			want: url.Values{"message": {"hello"}, "replyTo": {"42"}},
		},
		{
			name: "thread",
			req:  SendMessageRequest{Message: "hello", ThreadID: 7},
			want: url.Values{"message": {"hello"}, "threadId": {"7"}},
		},
		{
			// Talk rejects being given both, and derives the thread from the parent.
			name: "reply wins over thread",
			req:  SendMessageRequest{Message: "hello", ReplyTo: 42, ThreadID: 7},
			want: url.Values{"message": {"hello"}, "replyTo": {"42"}},
		},
		{
			name: "silent",
			req:  SendMessageRequest{Message: "hello", Silent: true},
			want: url.Values{"message": {"hello"}, "silent": {"true"}},
		},
		{
			// Talk truncates silently, so truncating here keeps the reference the
			// bridge remembers identical to the one Talk stored.
			name: "over-long reference is truncated",
			req:  SendMessageRequest{Message: "hello", ReferenceID: longRef},
			want: url.Values{"message": {"hello"}, "referenceId": {longRef[:maxReferenceIDLength]}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.form().Encode(); got != tc.want.Encode() {
				t.Errorf("form = %s, want %s", got, tc.want.Encode())
			}
		})
	}
}

func TestSendMessageErrors(t *testing.T) {
	tests := []struct {
		name       string
		httpStatus int
		ocsStatus  int
		check      func(error) bool
		checkName  string
	}{
		{"bad request", http.StatusBadRequest, http.StatusBadRequest, IsBadRequest, "IsBadRequest"},
		{"too large", http.StatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge, IsTooLarge, "IsTooLarge"},
		{"forbidden", http.StatusForbidden, http.StatusForbidden, IsForbidden, "IsForbidden"},
		{"not found", http.StatusNotFound, http.StatusNotFound, IsNotFound, "IsNotFound"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeOCSError(w, tc.httpStatus, tc.ocsStatus, "nope")
			})
			_, err := client.SendMessage(context.Background(), "abc123", SendMessageRequest{Message: "hello"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !tc.check(err) {
				t.Errorf("%s(%v) = false", tc.checkName, err)
			}
		})
	}
}

// The status helpers must not classify unrelated errors, or a transport failure
// would be treated as a Talk rejection and silently retried.
func TestStatusHelpersIgnoreOtherErrors(t *testing.T) {
	err := errors.New("connection refused")
	for name, check := range map[string]func(error) bool{
		"IsBadRequest": IsBadRequest,
		"IsTooLarge":   IsTooLarge,
		"IsForbidden":  IsForbidden,
	} {
		if check(err) {
			t.Errorf("%s(%v) = true", name, err)
		}
	}
}

// A 401 arrives outside the OCS envelope, so the helpers have to match on the
// transport status too.
func TestSendMessageUnauthorized(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := client.SendMessage(context.Background(), "abc123", SendMessageRequest{Message: "hello"})
	if !IsUnauthorized(err) {
		t.Fatalf("IsUnauthorized(%v) = false", err)
	}
}
