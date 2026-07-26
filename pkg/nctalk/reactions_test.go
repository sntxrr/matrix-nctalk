package nctalk

import (
	"context"
	"net/http"
	"net/url"
	"testing"
)

func TestReact(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeOCS(t, w, map[string]any{"👍": []any{}})
	})

	if err := client.React(context.Background(), "abc123", 4711, "👍"); err != nil {
		t.Fatalf("React: %v", err)
	}
	if last.Method != http.MethodPost {
		t.Errorf("Method = %s, want POST", last.Method)
	}
	if want := SpreedAPI + "/api/v1/reaction/abc123/4711"; last.Path != want {
		t.Errorf("Path = %s, want %s", last.Path, want)
	}
	form, err := url.ParseQuery(last.Body)
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if form.Get("reaction") != "👍" {
		t.Errorf("reaction = %q", form.Get("reaction"))
	}
	if !last.HasAuth || last.User != testUser {
		t.Errorf("expected the login's own credentials, got %q (auth=%v)", last.User, last.HasAuth)
	}
}

// The emoji must survive as a query parameter rather than a body, since a
// urlencoded DELETE body is the path Talk's own clients do not take.
func TestUnreactSendsEmojiInQuery(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{})
	})

	if err := client.Unreact(context.Background(), "abc123", 4711, "🎉"); err != nil {
		t.Fatalf("Unreact: %v", err)
	}
	if last.Method != http.MethodDelete {
		t.Errorf("Method = %s, want DELETE", last.Method)
	}
	if want := SpreedAPI + "/api/v1/reaction/abc123/4711"; last.Path != want {
		t.Errorf("Path = %s, want %s", last.Path, want)
	}
	if last.Body != "" {
		t.Errorf("Body = %q, want no body", last.Body)
	}
	query, err := url.ParseQuery(last.Query)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if query.Get("reaction") != "🎉" {
		t.Errorf("reaction = %q", query.Get("reaction"))
	}
}

// A conversation token is user-supplied data that ends up in a URL path.
func TestReactEscapesToken(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{})
	})

	if err := client.React(context.Background(), "a b/c", 1, "👍"); err != nil {
		t.Fatalf("React: %v", err)
	}
	if want := SpreedAPI + "/api/v1/reaction/a b/c/1"; last.Path != want {
		t.Errorf("Path = %s, want %s", last.Path, want)
	}
}

func TestReactionErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{"not an emoji", http.StatusBadRequest, IsBadRequest},
		{"message gone", http.StatusNotFound, IsNotFound},
		{"cannot react here", http.StatusForbidden, IsForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeOCSError(w, tc.status, tc.status, "nope")
			})

			err := client.React(context.Background(), "abc123", 4711, "👍")
			if err == nil || !tc.check(err) {
				t.Errorf("React err = %v, want status %d", err, tc.status)
			}
			err = client.Unreact(context.Background(), "abc123", 4711, "👍")
			if err == nil || !tc.check(err) {
				t.Errorf("Unreact err = %v, want status %d", err, tc.status)
			}
		})
	}
}

func TestListReactions(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{
			"👍": []any{
				map[string]any{"actorType": "users", "actorId": "bob",
					"actorDisplayName": "Bob", "timestamp": 1700000000},
				map[string]any{"actorType": "guests", "actorId": "g1",
					"actorDisplayName": "Guest", "timestamp": 1700000001},
			},
		})
	})

	list, err := client.ListReactions(context.Background(), "abc123", 4711)
	if err != nil {
		t.Fatalf("ListReactions: %v", err)
	}
	if last.Method != http.MethodGet {
		t.Errorf("Method = %s, want GET", last.Method)
	}
	if want := SpreedAPI + "/api/v1/reaction/abc123/4711"; last.Path != want {
		t.Errorf("Path = %s, want %s", last.Path, want)
	}
	reactors := list["👍"]
	if len(reactors) != 2 {
		t.Fatalf("got %d reactors, want 2", len(reactors))
	}
	if reactors[0].ActorID != "bob" || reactors[0].ActorType != ActorUsers {
		t.Errorf("first reactor = %+v", reactors[0])
	}
	if reactors[0].Timestamp != 1700000000 {
		t.Errorf("timestamp = %d", reactors[0].Timestamp)
	}
	if reactors[1].ActorType != ActorGuests {
		t.Errorf("second reactor = %+v", reactors[1])
	}
}

// Talk sends {} for a message with nothing on it today, but its other maps come
// back as the PHP empty array. Both must decode to an empty list rather than an
// error, since an error here would leave stale reactions in Matrix forever.
func TestListReactionsEmptyForms(t *testing.T) {
	for name, data := range map[string]string{
		"empty object": "{}",
		"empty array":  "[]",
		"null":         "null",
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ocs":{"meta":{"status":"ok","statuscode":200,"message":"OK"},"data":` + data + `}}`))
			})

			list, err := client.ListReactions(context.Background(), "abc123", 4711)
			if err != nil {
				t.Fatalf("ListReactions: %v", err)
			}
			if len(list) != 0 {
				t.Errorf("got %v, want no reactions", list)
			}
		})
	}
}

func TestListReactionsRejectsGarbage(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"👍": "not a list"})
	})

	if _, err := client.ListReactions(context.Background(), "abc123", 4711); err == nil {
		t.Fatal("expected an error for an unexpected reaction payload")
	}
}
