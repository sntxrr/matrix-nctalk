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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/sntxrr/matrix-nctalk/pkg/nctalk"
)

// signBodyWith produces the signature header for a body under a given secret.
func signBodyWith(secret, random, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(random + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// newMultiTenantConnector builds a connector serving two Nextcloud servers,
// each with its own secret.
func newMultiTenantConnector(t *testing.T) *NCTalkConnector {
	t.Helper()
	return &NCTalkConnector{
		Bridge: &bridgev2.Bridge{Log: zerolog.Nop()},
		Config: Config{
			// Left over from before per-host secrets were configured, as it
			// would be after an upgrade. It must no longer apply to anything.
			BotSecret: "the-old-global-secret-0123456789abcdef",
			BotSecrets: map[string]string{
				"cloud-a.example": "secret-for-a-0123456789abcdef0123456789",
				"cloud-b.example": "secret-for-b-0123456789abcdef0123456789",
			},
		},
		queue:  make(chan *pendingEvent, 8),
		nonces: newNonceCache(defaultNonceRetention, defaultNonceMaxSize),
	}
}

func TestWebhookSecretIsScopedToTheHostThatSentIt(t *testing.T) {
	nc := newMultiTenantConnector(t)
	body := `{"type":"Create","actor":{"type":"Person","id":"users/alice"},` +
		`"object":{"type":"Note","id":"1","name":"message","content":"{}"},` +
		`"target":{"type":"Collection","id":"tok"}}`

	// A's own server, signed with A's secret: accepted.
	random := strings.Repeat("a", 64)
	rec := postWebhook(t, nc, body, random, signBodyWith(nc.Config.BotSecrets["cloud-a.example"], random, body), "https://cloud-a.example/")
	if rec.Code != http.StatusOK {
		t.Fatalf("a legitimate webhook from cloud-a got %d", rec.Code)
	}

	// The same server claiming to be B, still signed with A's secret. Before
	// the secret was chosen by host this was accepted, because one shared
	// secret plus an unsigned backend header let any server speak for any
	// other.
	random = strings.Repeat("b", 64)
	rec = postWebhook(t, nc, body, random, signBodyWith(nc.Config.BotSecrets["cloud-a.example"], random, body), "https://cloud-b.example/")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("cloud-a was able to speak for cloud-b: got %d, want 401", rec.Code)
	}

	// A host with no entry of its own, signed with the leftover global secret.
	// This is the case that says the per-host secrets are load-bearing rather
	// than decorative: if bot_secret still applied to unlisted hosts, anyone
	// holding it could speak for a server the bridge has never been told about.
	random = strings.Repeat("c", 64)
	rec = postWebhook(t, nc, body, random, signBodyWith(nc.Config.BotSecret, random, body), "https://cloud-c.example/")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("the leftover global secret still worked for an unlisted host: got %d, want 401", rec.Code)
	}
}

func TestWebhookRejectsAReplayedRequest(t *testing.T) {
	nc := newTestConnector(t, 8)
	body := `{"type":"Create","actor":{"type":"Person","id":"users/alice"},` +
		`"object":{"type":"Note","id":"1","name":"message","content":"{}"},` +
		`"target":{"type":"Collection","id":"tok"}}`
	random := strings.Repeat("d", 64)
	signature := signBody(random, body)

	if rec := postWebhook(t, nc, body, random, signature, testBackend); rec.Code != http.StatusOK {
		t.Fatalf("first delivery got %d, want 200", rec.Code)
	}
	// Talk never reuses a random, so the same one again is a replay — which
	// before this was accepted, and for a reaction meant another fetch against
	// the Nextcloud server every time.
	if rec := postWebhook(t, nc, body, random, signature, testBackend); rec.Code != http.StatusUnauthorized {
		t.Errorf("replay got %d, want 401", rec.Code)
	}
	// A different random with the same body is a genuine new event.
	other := strings.Repeat("e", 64)
	if rec := postWebhook(t, nc, body, other, signBody(other, body), testBackend); rec.Code != http.StatusOK {
		t.Errorf("a fresh random got %d, want 200", rec.Code)
	}
}

func TestWebhookRejectsMissingHeadersWithoutReadingTheBody(t *testing.T) {
	nc := newTestConnector(t, 8)

	// Reading and hashing the body is the work an unauthenticated flood is
	// trying to buy, so the status code alone does not answer the question —
	// a rejection that happens after the read costs just as much. This watches
	// the body instead.
	for _, tc := range []struct {
		name      string
		random    string
		signature string
	}{
		{name: "no headers at all", random: "", signature: ""},
		// A plausible random with no signature is the case only the header
		// check catches: an empty one is stopped by the replay cache, which
		// runs before the read too, so it cannot tell the two apart.
		{name: "random but no signature", random: strings.Repeat("f", 64), signature: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &watchedReader{data: strings.Repeat("x", 4096)}
			req := httptest.NewRequest(http.MethodPost, "/webhook", body)
			req.Header.Set(nctalk.HeaderBackend, testBackend)
			if tc.random != "" {
				req.Header.Set(nctalk.HeaderRandom, tc.random)
			}
			if tc.signature != "" {
				req.Header.Set(nctalk.HeaderSignature, tc.signature)
			}
			rec := httptest.NewRecorder()
			nc.handleWebhook(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("got %d, want 401", rec.Code)
			}
			if body.read {
				t.Error("the body was read before the request was turned away")
			}
		})
	}
}

// watchedReader records whether anything read from it.
type watchedReader struct {
	data string
	pos  int
	read bool
}

func (w *watchedReader) Read(p []byte) (int, error) {
	w.read = true
	if w.pos >= len(w.data) {
		return 0, io.EOF
	}
	n := copy(p, w.data[w.pos:])
	w.pos += n
	return n, nil
}

func TestNonceCacheStaysBounded(t *testing.T) {
	cache := newNonceCache(time.Hour, 100)
	for i := range 1000 {
		if !cache.Accept(fmt.Sprintf("nonce-%d", i)) {
			t.Fatalf("nonce %d was wrongly treated as a replay", i)
		}
	}
	size, rotations, _ := cache.stats()
	// Two generations of at most maxSize each is the ceiling; without rotation
	// a flood would grow the map without end.
	if size > 200 {
		t.Errorf("cache holds %d entries, want at most two generations of 100", size)
	}
	if rotations == 0 {
		t.Error("the cache never rotated, so nothing bounds it")
	}
}

func TestNonceCacheRotatesOnAge(t *testing.T) {
	cache := newNonceCache(time.Minute, 1000)
	now := time.Unix(1700000000, 0)
	cache.now = func() time.Time { return now }

	cache.Accept("old")
	if cache.Accept("old") {
		t.Fatal("an immediate repeat should be a replay")
	}
	// One retention period keeps it in the older generation.
	now = now.Add(90 * time.Second)
	if cache.Accept("old") {
		t.Error("a nonce one generation old should still be remembered")
	}
	// Two rotations later it has been forgotten, which is the accepted cost of
	// bounding memory without timing out each entry.
	now = now.Add(90 * time.Second)
	cache.Accept("unrelated")
	now = now.Add(90 * time.Second)
	cache.Accept("unrelated2")
	if !cache.Accept("old") {
		t.Error("a nonce older than two generations should have been dropped")
	}
}

// stubResolver answers name lookups from a fixed table.
type stubResolver map[string][]net.IPAddr

func (s stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addrs, ok := s[host]
	if !ok {
		return nil, fmt.Errorf("no such host %q", host)
	}
	return addrs, nil
}

func TestCheckServerAddressRefusesInternalTargets(t *testing.T) {
	nc := &NCTalkConnector{dnsResolver: stubResolver{
		// A public name that quietly points inside, which is the interesting
		// case: nothing about the string gives it away.
		"sneaky.example": {{IP: net.ParseIP("10.1.2.3")}},
		"real.example":   {{IP: net.ParseIP("93.184.216.34")}},
	}}

	for _, tc := range []struct {
		name    string
		host    string
		wantErr bool
	}{
		{name: "loopback literal", host: "127.0.0.1:8081", wantErr: true},
		{name: "localhost by name", host: "localhost:8081", wantErr: true},
		{name: "private literal", host: "192.168.1.10", wantErr: true},
		// The one that matters most: cloud metadata services live here.
		{name: "link-local metadata", host: "169.254.169.254", wantErr: true},
		{name: "IPv6 loopback", host: "[::1]:8081", wantErr: true},
		{name: "IPv6 unique local", host: "[fd00::1]", wantErr: true},
		{name: "unspecified", host: "0.0.0.0", wantErr: true},
		{name: "name resolving inside", host: "sneaky.example", wantErr: true},
		{name: "ordinary public name", host: "real.example", wantErr: false},
		{name: "public literal", host: "93.184.216.34", wantErr: false},
		// A name that does not resolve is left to fail as a connection error,
		// which reports something more useful than this check could.
		{name: "unresolvable", host: "nowhere.invalid", wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := nc.checkServerAddress(context.Background(), tc.host)
			if tc.wantErr && err == nil {
				t.Errorf("%s was allowed", tc.host)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%s was refused: %v", tc.host, err)
			}
		})
	}
}

func TestCheckServerAddressHonoursTheAllowlist(t *testing.T) {
	// Naming an internal host is the operator saying they meant it, which is
	// what lets a bridge sit beside its own Nextcloud with no extra config.
	nc := &NCTalkConnector{Config: Config{AllowedServers: []string{"localhost:8081"}}}

	if err := nc.checkServerAddress(context.Background(), "localhost:8081"); err != nil {
		t.Errorf("an explicitly allowed internal host was refused: %v", err)
	}
	// Allowing one does not allow the rest of the private network.
	if err := nc.checkServerAddress(context.Background(), "127.0.0.1:9999"); err == nil {
		t.Error("allowlisting one host should not open the others")
	}
}

func TestBotSecretForIsolatesHosts(t *testing.T) {
	multi := &Config{
		BotSecret:  "global-secret",
		BotSecrets: map[string]string{"cloud-a.example": "secret-a"},
	}
	if got := multi.BotSecretFor("cloud-a.example"); got != "secret-a" {
		t.Errorf("got %q for a configured host", got)
	}
	// The global secret must not stand in for a host that has none, or the
	// per-host secrets would be decorative.
	if got := multi.BotSecretFor("cloud-b.example"); got != "" {
		t.Errorf("got %q for an unlisted host, want none", got)
	}
	if got := multi.BotSecretFor("CLOUD-A.EXAMPLE"); got != "secret-a" {
		t.Errorf("host matching should be case-insensitive, got %q", got)
	}

	single := &Config{BotSecret: "global-secret"}
	if got := single.BotSecretFor("anything.example"); got != "global-secret" {
		t.Errorf("a single-server bridge should use bot_secret, got %q", got)
	}
	if !single.HasBotSecret() || (&Config{}).HasBotSecret() {
		t.Error("HasBotSecret does not reflect what is configured")
	}
}

func TestDavPathRefusesTraversal(t *testing.T) {
	// Mirrors the nctalk-level test, from the caller's side: a path out of the
	// user's files must fail before a request is built, not be left for the
	// server to normalise.
	client := nctalk.NewClient("https://cloud.example.com", "alice", "pw")
	if _, err := client.DownloadFile(context.Background(), "../../../etc/passwd", 1024); err == nil {
		t.Error("a traversing path was accepted")
	}
}

func TestSingleSecretStillHonoursTheAllowlist(t *testing.T) {
	// One secret means one server holds it, so the secret is most of the trust
	// boundary — but where the operator has named their servers, a backend they
	// never mentioned has no business being acted on.
	listed := &Config{BotSecret: "s", AllowedServers: []string{"cloud.example.com"}}
	if got := listed.BotSecretFor("cloud.example.com"); got != "s" {
		t.Errorf("the named host got %q, want the secret", got)
	}
	if got := listed.BotSecretFor("attacker.example"); got != "" {
		t.Errorf("an unnamed host got %q, want none", got)
	}

	// An empty allowlist keeps the old behaviour, so a bridge that never
	// configured one does not stop receiving webhooks on upgrade.
	open := &Config{BotSecret: "s"}
	if got := open.BotSecretFor("anything.example"); got != "s" {
		t.Errorf("with no allowlist any backend should use the secret, got %q", got)
	}
}
