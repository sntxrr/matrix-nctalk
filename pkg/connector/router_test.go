package connector

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
)

// conversationServer answers GetConversation for the listed tokens and 404s for
// everything else, which is how Talk signals non-participation.
func conversationServer(t *testing.T, memberOf ...string) string {
	t.Helper()
	allowed := make(map[string]bool, len(memberOf))
	for _, token := range memberOf {
		allowed[token] = true
	}

	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		token := parts[len(parts)-1]
		if r.URL.Path == "/ocs/v2.php/apps/spreed/api/v4/room" {
			var convs []map[string]any
			for tok := range allowed {
				convs = append(convs, map[string]any{"token": tok, "type": 2, "displayName": tok})
			}
			writeOCS(t, w, convs)
			return
		}
		if !allowed[token] {
			writeOCSError(w, http.StatusNotFound, "Conversation not found")
			return
		}
		writeOCS(t, w, map[string]any{"token": token, "type": 2, "displayName": token})
	})
	return url
}

func TestRouterResolvesParticipatingLogin(t *testing.T) {
	url := conversationServer(t, "abc123")
	client := newTestClient(t, url, "alice", Config{})
	host := client.host()

	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	got, err := router.Resolve(context.Background(), host, "abc123")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if got != client.UserLogin {
		t.Fatalf("resolved to %v, want the participating login", got)
	}
}

// A conversation nobody has bridged is a normal condition, not an error: the
// bot may be enabled in conversations no bridge user is in.
func TestRouterReturnsNilWhenNoParticipant(t *testing.T) {
	url := conversationServer(t, "abc123")
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	got, err := router.Resolve(context.Background(), client.host(), "unbridged")
	if err != nil {
		t.Fatalf("Resolve returned an error for an unbridged conversation: %v", err)
	}
	if got != nil {
		t.Fatalf("resolved to %v, want nil", got)
	}
}

func TestRouterReturnsNilWhenNoLoginsForHost(t *testing.T) {
	router := newLoginRouter(&fakeLogins{})
	got, err := router.Resolve(context.Background(), "other.example.com", "abc123")
	if err != nil || got != nil {
		t.Fatalf("Resolve = (%v, %v), want (nil, nil)", got, err)
	}
}

// The chosen owner must be stable so a portal does not change hands between
// restarts; the lowest login ID wins.
func TestRouterChoosesLowestLoginIDDeterministically(t *testing.T) {
	url := conversationServer(t, "abc123")
	alice := newTestClient(t, url, "alice", Config{})
	bob := newTestClient(t, url, "bob", Config{})
	zoe := newTestClient(t, url, "zoe", Config{})
	host := alice.host()

	// Present the logins in a different order each time; the answer must not move.
	orders := [][]*bridgev2.UserLogin{
		{zoe.UserLogin, alice.UserLogin, bob.UserLogin},
		{bob.UserLogin, zoe.UserLogin, alice.UserLogin},
		{alice.UserLogin, bob.UserLogin, zoe.UserLogin},
	}
	for i, order := range orders {
		router := newLoginRouter(&fakeLogins{logins: order})
		got, err := router.Resolve(context.Background(), host, "abc123")
		if err != nil {
			t.Fatalf("order %d: Resolve failed: %v", i, err)
		}
		if got.ID != alice.UserLogin.ID {
			t.Errorf("order %d: resolved to %q, want %q", i, got.ID, alice.UserLogin.ID)
		}
	}
}

func TestRouterCachesResolution(t *testing.T) {
	var calls int
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeOCS(t, w, map[string]any{"token": "abc123", "type": 2, "displayName": "Project"})
	})
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	for range 3 {
		if _, err := router.Resolve(context.Background(), client.host(), "abc123"); err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("probed Nextcloud %d times, want 1 (result should be cached)", calls)
	}
}

// A busy conversation nobody is in must not re-probe on every message.
func TestRouterCachesNonParticipation(t *testing.T) {
	var calls int
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeOCSError(w, http.StatusNotFound, "Conversation not found")
	})
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	for range 3 {
		got, err := router.Resolve(context.Background(), client.host(), "unbridged")
		if err != nil || got != nil {
			t.Fatalf("Resolve = (%v, %v), want (nil, nil)", got, err)
		}
	}
	if calls != 1 {
		t.Errorf("probed Nextcloud %d times, want 1 (non-participation should be cached)", calls)
	}
}

// A transient failure must not be cached as non-participation, or the
// conversation would stay dark until restart.
func TestRouterDoesNotCacheTransientErrors(t *testing.T) {
	var calls int
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			writeOCSError(w, http.StatusInternalServerError, "Server error")
			return
		}
		writeOCS(t, w, map[string]any{"token": "abc123", "type": 2, "displayName": "Project"})
	})
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	if got, _ := router.Resolve(context.Background(), client.host(), "abc123"); got != nil {
		t.Fatal("expected no resolution while the server is erroring")
	}
	got, err := router.Resolve(context.Background(), client.host(), "abc123")
	if err != nil {
		t.Fatalf("second Resolve failed: %v", err)
	}
	if got == nil {
		t.Fatal("a transient error was cached as non-participation")
	}
}

func TestRouterIgnoresLoginsFromOtherHosts(t *testing.T) {
	url := conversationServer(t, "abc123")
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	got, err := router.Resolve(context.Background(), "different.example.com", "abc123")
	if err != nil || got != nil {
		t.Fatalf("Resolve = (%v, %v), want (nil, nil) for a different host", got, err)
	}
}

func TestRouterHostMatchIsCaseInsensitive(t *testing.T) {
	url := conversationServer(t, "abc123")
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	got, err := router.Resolve(context.Background(), strings.ToUpper(client.host()), "abc123")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if got == nil {
		t.Error("host matching should be case-insensitive")
	}
}

func TestRouterSkipsLoginsWithoutClient(t *testing.T) {
	url := conversationServer(t, "abc123")
	good := newTestClient(t, url, "bob", Config{})

	// A login that has not finished loading has no client yet.
	broken := newTestClient(t, url, "alice", Config{})
	broken.UserLogin.Client = nil

	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{broken.UserLogin, good.UserLogin}})
	got, err := router.Resolve(context.Background(), good.host(), "abc123")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if got == nil || got.ID != good.UserLogin.ID {
		t.Errorf("resolved to %v, want the login with a client", got)
	}
}

func TestRouterInvalidate(t *testing.T) {
	var calls int
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeOCS(t, w, map[string]any{"token": "abc123", "type": 2, "displayName": "Project"})
	})
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	if _, err := router.Resolve(context.Background(), client.host(), "abc123"); err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	router.Invalidate(client.host(), "abc123")
	if _, err := router.Resolve(context.Background(), client.host(), "abc123"); err != nil {
		t.Fatalf("Resolve after invalidate failed: %v", err)
	}
	if calls != 2 {
		t.Errorf("probed %d times, want 2 (invalidate should force a re-probe)", calls)
	}
}

func TestRouterInvalidateLogin(t *testing.T) {
	url := conversationServer(t, "abc123")
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	if _, err := router.Resolve(context.Background(), client.host(), "abc123"); err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	router.InvalidateLogin(string(client.UserLogin.ID))

	router.mu.RLock()
	_, stillCached := router.primary[routeKey(client.host(), "abc123")]
	router.mu.RUnlock()
	if stillCached {
		t.Error("InvalidateLogin left a cached decision for the removed login")
	}
}

func TestRouterWarmPopulatesCache(t *testing.T) {
	var conversationCalls int
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ocs/v2.php/apps/spreed/api/v4/room" {
			writeOCS(t, w, []map[string]any{
				{"token": "abc123", "type": 2, "displayName": "Project"},
				{"token": "def456", "type": 1, "displayName": "Bob"},
				// The changelog conversation must not be bridged.
				{"token": "changelog", "type": 4, "displayName": "Changelog"},
			})
			return
		}
		conversationCalls++
		writeOCS(t, w, map[string]any{"token": "abc123", "type": 2})
	})
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})

	if err := router.Warm(context.Background(), client.UserLogin); err != nil {
		t.Fatalf("Warm failed: %v", err)
	}

	// Warmed conversations resolve without a participation probe.
	for _, token := range []string{"abc123", "def456"} {
		got, err := router.Resolve(context.Background(), client.host(), token)
		if err != nil || got == nil {
			t.Errorf("Resolve(%q) = (%v, %v), want the warmed login", token, got, err)
		}
	}
	if conversationCalls != 0 {
		t.Errorf("made %d participation probes after warming, want 0", conversationCalls)
	}

	router.mu.RLock()
	_, changelogCached := router.primary[routeKey(client.host(), "changelog")]
	router.mu.RUnlock()
	if changelogCached {
		t.Error("the changelog conversation should not be warmed into the cache")
	}
}

// Warming from a second login must not steal conversations from the login that
// already owns them, or portals would change hands as people log in.
func TestRouterWarmDoesNotDisplaceLowerLoginID(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, []map[string]any{{"token": "abc123", "type": 2, "displayName": "Project"}})
	})
	alice := newTestClient(t, url, "alice", Config{})
	zoe := newTestClient(t, url, "zoe", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{alice.UserLogin, zoe.UserLogin}})

	if err := router.Warm(context.Background(), alice.UserLogin); err != nil {
		t.Fatalf("warming alice failed: %v", err)
	}
	if err := router.Warm(context.Background(), zoe.UserLogin); err != nil {
		t.Fatalf("warming zoe failed: %v", err)
	}

	got, _ := router.Resolve(context.Background(), alice.host(), "abc123")
	if got == nil || got.ID != alice.UserLogin.ID {
		t.Errorf("owner = %v, want alice to keep ownership", got)
	}
}

func TestRouterWarmPropagatesError(t *testing.T) {
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusForbidden, "Nope")
	})
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{})

	if err := router.Warm(context.Background(), client.UserLogin); err == nil {
		t.Fatal("expected an error when the conversation list cannot be read")
	}
}

func TestRouterWarmRejectsLoginWithoutClient(t *testing.T) {
	url := conversationServer(t, "abc123")
	client := newTestClient(t, url, "alice", Config{})
	client.UserLogin.Client = nil

	router := newLoginRouter(&fakeLogins{})
	if err := router.Warm(context.Background(), client.UserLogin); err == nil {
		t.Error("expected an error for a login with no Nextcloud client")
	}
}

func TestRouterIsConcurrencySafe(t *testing.T) {
	url := conversationServer(t, "abc123", "def456")
	client := newTestClient(t, url, "alice", Config{})
	router := newLoginRouter(&fakeLogins{logins: []*bridgev2.UserLogin{client.UserLogin}})
	host := client.host()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := "abc123"
			if i%2 == 0 {
				token = "def456"
			}
			_, _ = router.Resolve(context.Background(), host, token)
			if i%5 == 0 {
				router.Invalidate(host, token)
			}
		}(i)
	}
	wg.Wait()
}
