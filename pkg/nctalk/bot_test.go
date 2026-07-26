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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// referenceSign is an independent reimplementation of what spreed's
// ChecksumVerificationService::validateRequest computes:
//
//	hash_hmac('sha256', $random . $data, $secret)
//
// Keeping it separate from sign() means the tests would catch a change to the
// production implementation rather than moving along with it.
func referenceSign(t *testing.T, random, data, secret string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(random + data))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhookAcceptsValidSignature(t *testing.T) {
	random := strings.Repeat("a", 64)
	body := []byte(`{"type":"Create","actor":{"type":"Person","id":"users/alice"}}`)

	sig := referenceSign(t, random, string(body), testSecret)
	if err := VerifyWebhook(random, sig, testSecret, body); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifyWebhookAcceptsUppercaseSignature(t *testing.T) {
	// spreed lowercases the received checksum before comparing, so a bridge
	// that only accepted lowercase would reject legitimate requests.
	random := strings.Repeat("b", 64)
	body := []byte(`{"type":"Like"}`)

	sig := strings.ToUpper(referenceSign(t, random, string(body), testSecret))
	if err := VerifyWebhook(random, sig, testSecret, body); err != nil {
		t.Fatalf("uppercase signature rejected: %v", err)
	}
}

func TestVerifyWebhookRejectsTamperedBody(t *testing.T) {
	random := strings.Repeat("c", 64)
	body := []byte(`{"type":"Create","object":{"content":"hello"}}`)
	sig := referenceSign(t, random, string(body), testSecret)

	tampered := []byte(`{"type":"Create","object":{"content":"hellp"}}`)
	err := VerifyWebhook(random, sig, testSecret, tampered)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body accepted, got err=%v", err)
	}
}

func TestVerifyWebhookRejectsWrongSecret(t *testing.T) {
	random := strings.Repeat("d", 64)
	body := []byte(`{"type":"Create"}`)
	sig := referenceSign(t, random, string(body), "a-different-secret-value-padding!")

	if err := VerifyWebhook(random, sig, testSecret, body); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("signature from wrong secret accepted, got err=%v", err)
	}
}

func TestVerifyWebhookRejectsShortRandom(t *testing.T) {
	// spreed throws UnauthorizedException for randoms under 32 characters, so
	// accepting them would let a caller weaken the construction.
	random := strings.Repeat("e", 31)
	body := []byte(`{"type":"Create"}`)
	sig := referenceSign(t, random, string(body), testSecret)

	if err := VerifyWebhook(random, sig, testSecret, body); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("short random accepted, got err=%v", err)
	}
}

func TestVerifyWebhookRejectsEmptyInputs(t *testing.T) {
	body := []byte(`{}`)
	random := strings.Repeat("f", 64)

	if err := VerifyWebhook(random, "", testSecret, body); !errors.Is(err, ErrBadSignature) {
		t.Error("empty signature accepted")
	}
	if err := VerifyWebhook(random, referenceSign(t, random, string(body), ""), "", body); !errors.Is(err, ErrBadSignature) {
		t.Error("empty secret accepted")
	}
}

func TestVerifyWebhookIsBodyByteExact(t *testing.T) {
	// The signature covers the exact bytes received, so re-encoded JSON that is
	// semantically identical must still fail. This is why the handler has to
	// read the raw body before decoding.
	random := strings.Repeat("0", 64)
	original := []byte(`{"a":1,"b":2}`)
	reencoded := []byte(`{"a": 1, "b": 2}`)

	sig := referenceSign(t, random, string(original), testSecret)
	if err := VerifyWebhook(random, sig, testSecret, reencoded); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("re-encoded body accepted, got err=%v", err)
	}
}

// TestOutgoingSignatureCoversOnlyTheMessage pins the asymmetry between the two
// directions: inbound signs the whole JSON body, outbound signs only the
// message text (or reaction emoji), never the encoded form body.
func TestOutgoingSignatureCoversOnlyTheMessage(t *testing.T) {
	random := strings.Repeat("9", 64)
	message := "hello from matrix"

	got := sign(random, message, testSecret)
	want := referenceSign(t, random, message, testSecret)
	if got != want {
		t.Fatalf("outgoing signature mismatch:\n got %s\nwant %s", got, want)
	}

	// The form encoding of the same message must NOT produce the same digest,
	// which is what makes signing the form body a real bug rather than a nit.
	formBody := "message=" + strings.ReplaceAll(message, " ", "+")
	if sign(random, formBody, testSecret) == want {
		t.Fatal("signing the form body produced the same digest as signing the message")
	}
}

func TestNewRandomMeetsTalkRequirements(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		r, err := newRandom()
		if err != nil {
			t.Fatalf("newRandom failed: %v", err)
		}
		if len(r) < minRandomLength {
			t.Fatalf("random too short for Talk: %d < %d", len(r), minRandomLength)
		}
		if seen[r] {
			t.Fatal("newRandom returned a duplicate value")
		}
		seen[r] = true
	}
}
