package connector

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

const (
	testBotSecret = "0123456789abcdef0123456789abcdef"
	testBackend   = "https://cloud.example.com/"
)

// newTestConnector builds the minimum connector the webhook handler touches:
// a logger, the bot secret, and the event queue.
func newTestConnector(t *testing.T, queueSize int) *NCTalkConnector {
	t.Helper()
	return &NCTalkConnector{
		Bridge: &bridgev2.Bridge{Log: zerolog.Nop()},
		Config: Config{BotSecret: testBotSecret},
		queue:  make(chan *pendingEvent, queueSize),
	}
}

// signBody produces the header value Talk would send for a given body.
func signBody(random, body string) string {
	mac := hmac.New(sha256.New, []byte(testBotSecret))
	mac.Write([]byte(random + body))
	return hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, nc *NCTalkConnector, body, random, signature, backend string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set(nctalk.HeaderRandom, random)
	req.Header.Set(nctalk.HeaderSignature, signature)
	req.Header.Set(nctalk.HeaderBackend, backend)

	rec := httptest.NewRecorder()
	nc.handleWebhook(rec, req)
	return rec
}

func TestWebhookAcceptsSignedEvent(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("a", 64)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, createFixtureJSON), testBackend)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	select {
	case pending := <-nc.queue:
		if pending.host != "cloud.example.com" {
			t.Errorf("host = %q, want cloud.example.com", pending.host)
		}
		if pending.evt.Type != nctalk.ActivityCreate {
			t.Errorf("activity = %q, want Create", pending.evt.Type)
		}
		if pending.receivedAt.IsZero() {
			t.Error("receivedAt was not set")
		}
	default:
		t.Fatal("event was not enqueued")
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("b", 64)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, "a different body"), testBackend)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(nc.queue) != 0 {
		t.Error("an unsigned event was enqueued")
	}
}

func TestWebhookRejectsMissingBackendHeader(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("c", 64)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, createFixtureJSON), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(nc.queue) != 0 {
		t.Error("an event with no backend was enqueued")
	}
}

// A validly signed but unparseable payload must still be acknowledged. Talk
// counts non-2xx responses against an error budget and disables the bot once it
// is exhausted, so a payload shape we do not understand must not cost us the
// whole integration.
func TestWebhookAcksUnparseablePayload(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("d", 64)
	body := `{"actor":{"id":"users/alice"}}` // valid JSON, no activity type

	rec := postWebhook(t, nc, body, random, signBody(random, body), testBackend)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so Talk does not disable the bot", rec.Code)
	}
	if len(nc.queue) != 0 {
		t.Error("an unparseable event was enqueued")
	}
}

// The same reasoning applies when the bridge cannot keep up: dropping an event
// is bad, but returning an error would eventually disable the bot entirely.
func TestWebhookAcksWhenQueueIsFull(t *testing.T) {
	nc := newTestConnector(t, 0) // unbuffered with no reader: always full
	random := strings.Repeat("e", 64)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, createFixtureJSON), testBackend)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when the queue is full", rec.Code)
	}
}

func TestWebhookRejectsOversizedBody(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("f", 64)
	huge := `{"type":"Create","padding":"` + strings.Repeat("x", maxWebhookBody+1) + `"}`

	rec := postWebhook(t, nc, huge, random, signBody(random, huge), testBackend)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(nc.queue) != 0 {
		t.Error("an oversized event was enqueued")
	}
}

func TestWebhookRejectsShortRandom(t *testing.T) {
	nc := newTestConnector(t, 4)
	random := strings.Repeat("g", 8)

	rec := postWebhook(t, nc, createFixtureJSON, random, signBody(random, createFixtureJSON), testBackend)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// createFixtureJSON is a single-line Create activity, kept compact so the
// signature covers exactly the bytes the test sends.
const createFixtureJSON = `{"type":"Create","actor":{"type":"Person","id":"users/alice","name":"Alice Example"},"object":{"type":"Note","id":"4711","name":"message","content":"{\"message\":\"hello\",\"parameters\":{}}","mediaType":"text/markdown"},"target":{"type":"Collection","id":"abc123token","name":"Project chat"},"published":"2026-07-26T12:00:00+00:00"}`
