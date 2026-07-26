package nctalk

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNewClientNormalisesBaseURL(t *testing.T) {
	c := NewClient("https://cloud.example.com/", "alice", "pw")
	if c.BaseURL != "https://cloud.example.com" {
		t.Errorf("BaseURL = %q, trailing slash should be trimmed", c.BaseURL)
	}
	if c.HTTP == nil {
		t.Error("expected a default HTTP client")
	}
}

func TestClientHost(t *testing.T) {
	tests := []struct{ base, want string }{
		{"https://cloud.example.com", "cloud.example.com"},
		{"https://cloud.example.com:8443", "cloud.example.com:8443"},
		{"https://example.com/nextcloud", "example.com"},
		// A value that cannot be parsed falls back to the raw string rather
		// than returning empty, so IDs built from it stay distinguishable.
		{"://broken", "://broken"},
	}
	for _, tc := range tests {
		if got := NewClient(tc.base, "", "").Host(); got != tc.want {
			t.Errorf("Host() for %q = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestRequestSendsRequiredHeadersAndAuth(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]string{"ok": "yes"})
	})

	if _, err := client.request(context.Background(), http.MethodGet, "/test", nil, nil); err != nil {
		t.Fatalf("request failed: %v", err)
	}

	// Nextcloud rejects OCS calls without this header, so it is not optional.
	if last.Header.Get("OCS-APIRequest") != "true" {
		t.Error("missing OCS-APIRequest header")
	}
	if last.Header.Get("Accept") != "application/json" {
		t.Error("missing JSON Accept header")
	}
	if last.Header.Get("User-Agent") != UserAgent {
		t.Errorf("User-Agent = %q, want %q", last.Header.Get("User-Agent"), UserAgent)
	}
	if !last.HasAuth || last.User != testUser || last.Pass != testPass {
		t.Errorf("basic auth not sent correctly: user=%q ok=%v", last.User, last.HasAuth)
	}
}

func TestRequestOmitsAuthWhenNoCredentials(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, nil)
	})
	client.Username = ""
	client.AppPassword = ""

	if _, err := client.request(context.Background(), http.MethodGet, "/test", nil, nil); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if last.HasAuth {
		t.Error("basic auth should not be sent when no credentials are configured")
	}
}

func TestRequestEncodesQueryAndForm(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, nil)
	})

	query := url.Values{"lookIntoFuture": {"1"}, "timeout": {"30"}}
	form := url.Values{"message": {"hello world"}}
	if _, err := client.request(context.Background(), http.MethodPost, "/chat", query, form); err != nil {
		t.Fatalf("request failed: %v", err)
	}

	if !strings.Contains(last.Query, "lookIntoFuture=1") || !strings.Contains(last.Query, "timeout=30") {
		t.Errorf("query not encoded: %q", last.Query)
	}
	if last.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form encoding", last.Header.Get("Content-Type"))
	}
	if last.Body != "message=hello+world" {
		t.Errorf("body = %q, want form-encoded message", last.Body)
	}
}

func TestRequestUnwrapsOCSEnvelope(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"token": "abc123", "type": 2})
	})

	var out struct {
		Token string `json:"token"`
		Type  int    `json:"type"`
	}
	if _, err := client.requestJSON(context.Background(), http.MethodGet, "/room", nil, nil, &out); err != nil {
		t.Fatalf("requestJSON failed: %v", err)
	}
	if out.Token != "abc123" || out.Type != 2 {
		t.Errorf("decoded %+v, want token=abc123 type=2", out)
	}
}

func TestRequestReturnsResponseHeaders(t *testing.T) {
	// The chat API conveys pagination cursors in headers, so they must survive
	// the envelope unwrapping.
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Chat-Last-Given", "4711")
		writeOCS(t, w, nil)
	})

	resp, err := client.request(context.Background(), http.MethodGet, "/chat/abc", nil, nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.Headers.Get("X-Chat-Last-Given") != "4711" {
		t.Error("response headers were not preserved")
	}
}

// Nextcloud commonly reports OCS failures with HTTP 200 and a failure status
// inside the envelope, so the client must read the envelope, not the status.
func TestRequestSurfacesOCSErrorDespiteHTTP200(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusOK, http.StatusNotFound, "Conversation not found")
	})

	_, err := client.request(context.Background(), http.MethodGet, "/room/nope", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var ocsErr *Error
	if !errors.As(err, &ocsErr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if ocsErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", ocsErr.StatusCode)
	}
	if ocsErr.Message != "Conversation not found" {
		t.Errorf("Message = %q", ocsErr.Message)
	}
	if !IsNotFound(err) {
		t.Error("IsNotFound should recognise this error")
	}
}

// The router relies on IsNotFound to detect that a user is not a participant,
// so a 404 arriving as a real HTTP status must be classified identically.
func TestIsNotFoundForHTTPStatus404(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusNotFound, http.StatusNotFound, "Not found")
	})

	_, err := client.GetConversation(context.Background(), "missing")
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound returned false for a 404: %v", err)
	}
}

// A 401 never carries a usable envelope, so it is reported directly rather
// than as a JSON parse failure.
func TestRequestHandlesUnauthorizedWithoutEnvelope(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<html>Not authorised</html>`))
	})

	_, err := client.request(context.Background(), http.MethodGet, "/test", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsUnauthorized(err) {
		t.Errorf("IsUnauthorized should recognise this error, got %v", err)
	}
	if IsNotFound(err) {
		t.Error("a 401 should not be classified as not-found")
	}
}

func TestRequestRejectsUnparseableBody(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>this is not OCS JSON</html>`))
	})

	_, err := client.request(context.Background(), http.MethodGet, "/test", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var ocsErr *Error
	if !errors.As(err, &ocsErr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if !strings.Contains(ocsErr.Message, "unparseable") {
		t.Errorf("Message = %q, want it to mention unparseable", ocsErr.Message)
	}
	if !strings.Contains(ocsErr.Body, "not OCS JSON") {
		t.Error("the raw body should be retained for diagnosis")
	}
}

func TestRequestJSONRejectsMismatchedPayload(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, []string{"an", "array"})
	})

	var out struct {
		Token string `json:"token"`
	}
	_, err := client.requestJSON(context.Background(), http.MethodGet, "/room", nil, nil, &out)
	if err == nil {
		t.Fatal("expected a decode error when the payload shape does not match")
	}
}

func TestRequestJSONTolerNilOut(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]string{"ignored": "value"})
	})
	if _, err := client.requestJSON(context.Background(), http.MethodPost, "/bot/abc/1", nil, nil, nil); err != nil {
		t.Fatalf("a nil out parameter should discard the payload, got %v", err)
	}
}

func TestRequestPropagatesTransportError(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", "u", "p")
	_, err := client.request(context.Background(), http.MethodGet, "/test", nil, nil)
	if err == nil {
		t.Fatal("expected a transport error against a closed port")
	}
}

func TestRequestHonoursContextCancellation(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, nil)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.request(ctx, http.MethodGet, "/test", nil, nil); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

func TestErrorMessageFormatting(t *testing.T) {
	withMessage := &Error{StatusCode: 404, HTTPStatus: 200, Message: "Not found"}
	if !strings.Contains(withMessage.Error(), "Not found") || !strings.Contains(withMessage.Error(), "404") {
		t.Errorf("Error() = %q", withMessage.Error())
	}

	withoutMessage := &Error{StatusCode: 500, HTTPStatus: 502}
	got := withoutMessage.Error()
	if !strings.Contains(got, "500") || !strings.Contains(got, "502") {
		t.Errorf("Error() = %q, want both status codes", got)
	}
}

func TestClassifiersIgnoreForeignErrors(t *testing.T) {
	other := errors.New("something else entirely")
	if IsNotFound(other) || IsUnauthorized(other) {
		t.Error("classifiers should only match *Error values")
	}
	if IsNotFound(nil) || IsUnauthorized(nil) {
		t.Error("classifiers should return false for nil")
	}
}

func TestTruncateLimitsLongBodies(t *testing.T) {
	short := truncate([]byte("short"))
	if short != "short" {
		t.Errorf("short body was altered: %q", short)
	}

	long := truncate([]byte(strings.Repeat("x", 1000)))
	if len([]rune(long)) > 513 {
		t.Errorf("long body was not truncated, got %d runes", len([]rune(long)))
	}
	if !strings.HasSuffix(long, "…") {
		t.Error("truncated body should be marked with an ellipsis")
	}
}
