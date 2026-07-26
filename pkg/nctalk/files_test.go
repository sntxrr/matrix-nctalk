package nctalk

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAttachmentFolder(t *testing.T) {
	tests := []struct {
		name string
		caps *Capabilities
		want string
	}{
		{"configured", &Capabilities{Config: map[string]any{
			"attachments": map[string]any{"folder": "/Chat files"}}}, "/Chat files"},
		{"no config at all", &Capabilities{}, DefaultAttachmentFolder},
		{"attachments absent", &Capabilities{Config: map[string]any{"other": 1}}, DefaultAttachmentFolder},
		{"folder empty", &Capabilities{Config: map[string]any{
			"attachments": map[string]any{"folder": ""}}}, DefaultAttachmentFolder},
		{"folder wrong type", &Capabilities{Config: map[string]any{
			"attachments": map[string]any{"folder": 7}}}, DefaultAttachmentFolder},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.caps.AttachmentFolder(); got != tc.want {
				t.Errorf("AttachmentFolder() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAttachmentsAllowed(t *testing.T) {
	tests := []struct {
		name string
		caps *Capabilities
		want bool
	}{
		{"allowed", &Capabilities{Config: map[string]any{
			"attachments": map[string]any{"allowed": true}}}, true},
		{"disallowed", &Capabilities{Config: map[string]any{
			"attachments": map[string]any{"allowed": false}}}, false},
		// Servers old enough not to advertise the setting do allow attachments,
		// so silence must not read as a refusal.
		{"not advertised", &Capabilities{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.caps.AttachmentsAllowed(); got != tc.want {
				t.Errorf("AttachmentsAllowed() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A file name is attacker-influenced data that ends up in a URL path, so every
// segment is escaped individually and the separators are the only raw slashes.
func TestDavPathEscaping(t *testing.T) {
	client := NewClient("https://cloud.example.com", "user name", "pw")
	got := client.davPath("/Talk/holiday photo #1?.png")
	want := "https://cloud.example.com/remote.php/dav/files/user%20name/Talk/holiday%20photo%20%231%3F.png"
	if got != want {
		t.Errorf("davPath =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEnsureFolder(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"created", http.StatusCreated, false},
		// WebDAV answers 405 when the collection is already there, which is the
		// desired end state rather than a failure.
		{"already exists", http.StatusMethodNotAllowed, false},
		{"forbidden", http.StatusForbidden, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			err := client.EnsureFolder(context.Background(), "/Talk")
			if (err != nil) != tc.wantErr {
				t.Fatalf("EnsureFolder err = %v, wantErr %v", err, tc.wantErr)
			}
			if last.Method != "MKCOL" {
				t.Errorf("Method = %s, want MKCOL", last.Method)
			}
			if want := "/remote.php/dav/files/" + testUser + "/Talk"; last.Path != want {
				t.Errorf("Path = %s, want %s", last.Path, want)
			}
		})
	}
}

func TestUploadAndDownloadFile(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusCreated)
			return
		}
		_, _ = w.Write([]byte("file contents"))
	})

	if err := client.UploadFile(context.Background(), "/Talk/a.txt", []byte("file contents"), "text/plain"); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if last.Method != http.MethodPut {
		t.Errorf("Method = %s, want PUT", last.Method)
	}
	if last.Body != "file contents" {
		t.Errorf("uploaded %q", last.Body)
	}
	if got := last.Header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q", got)
	}
	if !last.HasAuth || last.User != testUser {
		t.Errorf("expected the login's credentials, got %q (auth=%v)", last.User, last.HasAuth)
	}
	data, err := client.DownloadFile(context.Background(), "/Talk/a.txt", 1024)
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if string(data) != "file contents" {
		t.Errorf("downloaded %q", data)
	}
}

// A file larger than the caller will accept must fail rather than being
// silently truncated into a corrupt upload.
func TestDownloadFileRefusesOversize(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 200)))
	})

	_, err := client.DownloadFile(context.Background(), "/Talk/big.bin", 100)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want a size limit error", err)
	}
}

func TestUploadFileRejection(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInsufficientStorage)
	})

	err := client.UploadFile(context.Background(), "/Talk/a.txt", []byte("x"), "text/plain")
	if err == nil {
		t.Fatal("expected an error when the server refuses the upload")
	}
}

func TestDownloadFileRejection(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	if _, err := client.DownloadFile(context.Background(), "/Talk/gone.txt", 1024); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// WebDAV PUT overwrites without complaint, so a name already in use has to be
// stepped around before uploading, not discovered afterwards.
func TestFreeFilePathAvoidsCollisions(t *testing.T) {
	taken := map[string]bool{
		"/remote.php/dav/files/" + testUser + "/Talk/photo.png":     true,
		"/remote.php/dav/files/" + testUser + "/Talk/photo (2).png": true,
	}
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if taken[r.URL.Path] {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	got, err := client.FreeFilePath(context.Background(), "/Talk", "photo.png")
	if err != nil {
		t.Fatalf("FreeFilePath: %v", err)
	}
	if got != "/Talk/photo (3).png" {
		t.Errorf("FreeFilePath = %q, want /Talk/photo (3).png", got)
	}
}

func TestFreeFilePathGivesUp(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // everything is taken
	})

	if _, err := client.FreeFilePath(context.Background(), "/Talk", "a.txt"); err == nil {
		t.Fatal("expected FreeFilePath to give up rather than loop forever")
	}
}

// Names come from another network, so they must not be able to climb out of the
// attachment folder or produce something Nextcloud will not store.
func TestSanitiseFileName(t *testing.T) {
	tests := map[string]string{
		"../../etc/passwd": "_.._etc_passwd",
		"a/b.txt":          "a_b.txt",
		`a\b.txt`:          "a_b.txt",
		".hidden":          "hidden",
		"  spaced.txt  ":   "spaced.txt",
		"":                 "file",
		"...":              "file",
	}
	for in, want := range tests {
		if got := sanitiseFileName(in); got != want {
			t.Errorf("sanitiseFileName(%q) = %q, want %q", in, got, want)
		}
	}

	long := sanitiseFileName(strings.Repeat("n", 400) + ".png")
	if len(long) > 200 {
		t.Errorf("long name is %d characters, want it truncated", len(long))
	}
	if !strings.HasSuffix(long, ".png") {
		t.Errorf("truncated name %q lost its extension", long[len(long)-10:])
	}
}

func TestShareFileToConversation(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": "7", "file_source": 200})
	})

	err := client.ShareFileToConversation(context.Background(), ShareFileRequest{
		Path:        "/Talk/note.txt",
		Token:       "abc123",
		ReferenceID: "ref-1",
		Caption:     "look at this",
	})
	if err != nil {
		t.Fatalf("ShareFileToConversation: %v", err)
	}
	if want := "/ocs/v2.php/apps/files_sharing/api/v1/shares"; last.Path != want {
		t.Errorf("Path = %s, want %s", last.Path, want)
	}
	form, err := url.ParseQuery(last.Body)
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	// Share type 10 is what makes this a chat message rather than only a file
	// appearing in the recipient's Files app.
	if form.Get("shareType") != "10" {
		t.Errorf("shareType = %q, want 10", form.Get("shareType"))
	}
	if form.Get("shareWith") != "abc123" {
		t.Errorf("shareWith = %q", form.Get("shareWith"))
	}
	if form.Get("path") != "/Talk/note.txt" {
		t.Errorf("path = %q", form.Get("path"))
	}
	// The reference is the only way to find the message Talk creates, since the
	// share response does not report its ID.
	if form.Get("referenceId") != "ref-1" {
		t.Errorf("referenceId = %q", form.Get("referenceId"))
	}
	if got := form.Get("talkMetaData"); got != `{"caption":"look at this"}` {
		t.Errorf("talkMetaData = %q", got)
	}
}

func TestShareFileWithoutCaption(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": "7"})
	})

	err := client.ShareFileToConversation(context.Background(), ShareFileRequest{
		Path: "/Talk/note.txt", Token: "abc123",
	})
	if err != nil {
		t.Fatalf("ShareFileToConversation: %v", err)
	}
	form, _ := url.ParseQuery(last.Body)
	if _, ok := form["talkMetaData"]; ok {
		t.Error("a message with no caption should not send talkMetaData")
	}
}

// Talk truncates the reference it stores, so sending a longer one would leave
// the client looking for a string the server never kept.
func TestShareFileTruncatesReference(t *testing.T) {
	client, last := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCS(t, w, map[string]any{"id": "7"})
	})

	long := strings.Repeat("a", maxReferenceIDLength+20)
	err := client.ShareFileToConversation(context.Background(), ShareFileRequest{
		Path: "/Talk/n.txt", Token: "abc123", ReferenceID: long,
	})
	if err != nil {
		t.Fatalf("ShareFileToConversation: %v", err)
	}
	form, _ := url.ParseQuery(last.Body)
	if got := form.Get("referenceId"); len(got) != maxReferenceIDLength {
		t.Errorf("referenceId is %d characters, want %d", len(got), maxReferenceIDLength)
	}
}

func TestShareFileRejection(t *testing.T) {
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOCSError(w, http.StatusForbidden, http.StatusForbidden, "not allowed")
	})

	err := client.ShareFileToConversation(context.Background(), ShareFileRequest{
		Path: "/Talk/n.txt", Token: "abc123",
	})
	if !IsForbidden(err) {
		t.Fatalf("IsForbidden(%v) = false", err)
	}
}
