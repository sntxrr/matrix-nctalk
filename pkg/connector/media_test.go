package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// stubDownloader stands in for the bridge bot's media repository access.
type stubDownloader struct {
	data []byte
	err  error
}

func (s *stubDownloader) DownloadMedia(context.Context, id.ContentURIString, *event.EncryptedFileInfo) ([]byte, error) {
	return s.data, s.err
}

// stubUploader records what the connector pushed into the Matrix media repo.
// Only UploadMedia is exercised, so the rest of MatrixAPI is left unimplemented
// and would panic loudly rather than silently returning zero values.
type stubUploader struct {
	bridgev2.MatrixAPI
	mu       sync.Mutex
	uploaded []byte
	name     string
	mimeType string
	err      error
}

func (s *stubUploader) UploadMedia(_ context.Context, _ id.RoomID, data []byte, fileName, mimeType string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", nil, s.err
	}
	s.uploaded, s.name, s.mimeType = data, fileName, mimeType
	return id.ContentURIString("mxc://matrix.example.com/abc"), nil, nil
}

func TestMsgTypeForMime(t *testing.T) {
	tests := map[string]event.MessageType{
		"image/png":                event.MsgImage,
		"video/mp4":                event.MsgVideo,
		"audio/ogg":                event.MsgAudio,
		"application/pdf":          event.MsgFile,
		"text/plain":               event.MsgFile,
		"application/octet-stream": event.MsgFile,
	}
	for mimeType, want := range tests {
		if got := msgTypeForMime(mimeType); got != want {
			t.Errorf("msgTypeForMime(%q) = %q, want %q", mimeType, got, want)
		}
	}
}

func TestFileParamKey(t *testing.T) {
	tests := []struct {
		name   string
		params nctalk.MessageParams
		want   string
	}{
		{"conventional key", nctalk.MessageParams{
			"file": {Type: nctalk.ParamTypeFile, Name: "a.txt"}}, "file"},
		{"other key", nctalk.MessageParams{
			"attachment": {Type: nctalk.ParamTypeFile, Name: "a.txt"}}, "attachment"},
		{"no file", nctalk.MessageParams{
			"actor": {Type: nctalk.ParamTypeUser, Name: "Alice"}}, ""},
		{"key named file but not a file", nctalk.MessageParams{
			"file": {Type: nctalk.ParamTypeUser, Name: "Alice"}}, ""},
		{"empty", nctalk.MessageParams{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileParamKey(tc.params); got != tc.want {
				t.Errorf("fileParamKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// fileServer answers the calls an incoming file share makes: the message
// context lookup that resolves the path, and the WebDAV download.
func fileServer(t *testing.T, file map[string]any, contents string) string {
	t.Helper()
	url, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/context"):
			writeOCS(t, w, []map[string]any{{
				"id":                4711,
				"token":             "abc123token",
				"message":           "{file}",
				"messageParameters": map[string]any{"file": file},
			}})
		case strings.HasPrefix(r.URL.Path, "/remote.php/dav/"):
			_, _ = w.Write([]byte(contents))
		default:
			writeOCS(t, w, map[string]any{"token": "abc123token"})
		}
	})
	return url
}

// incomingFileMessage builds the Talk message a file share produces.
func incomingFileMessage() *talkMessage {
	return &talkMessage{
		Token:     "abc123token",
		MessageID: 4711,
		ActorType: nctalk.ActorUsers,
		ActorID:   "bob",
		Text:      "{file}",
		// The webhook's copy of the path is not resolved for anyone, which is
		// exactly why the conversion refuses to use it.
		Parameters: map[string]nctalk.MessageParam{
			"file": {Type: nctalk.ParamTypeFile, ID: "200", Name: "note.txt", Path: "note.txt"},
		},
	}
}

func TestConvertMessageBridgesSharedFile(t *testing.T) {
	serverURL := fileServer(t, map[string]any{
		"type": "file", "id": "200", "name": "note.txt",
		"path": "Talk/note.txt", "mimetype": "text/plain", "size": "13",
	}, "file contents")
	client := newTestClient(t, serverURL, "alice", Config{})
	uploader := &stubUploader{}

	converted, err := client.convertMessage(context.Background(),
		newTestPortal(client.host(), "abc123token"), uploader, incomingFileMessage())
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	if len(converted.Parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(converted.Parts))
	}

	content := converted.Parts[0].Content
	if content.MsgType != event.MsgFile {
		t.Errorf("MsgType = %q, want a file", content.MsgType)
	}
	if content.Body != "note.txt" {
		t.Errorf("Body = %q", content.Body)
	}
	if content.URL == "" {
		t.Error("no media URL was set")
	}
	if content.Info == nil || content.Info.MimeType != "text/plain" {
		t.Errorf("Info = %+v", content.Info)
	}
	if string(uploader.uploaded) != "file contents" {
		t.Errorf("uploaded %q", uploader.uploaded)
	}
	if uploader.name != "note.txt" {
		t.Errorf("uploaded under the name %q", uploader.name)
	}
}

// The path in the webhook payload is resolved for nobody and does not exist in
// the login's files, so the conversion has to re-ask Talk for the real one.
func TestConvertMessageResolvesFilePath(t *testing.T) {
	var davPaths []string
	var mu sync.Mutex
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/context"):
			writeOCS(t, w, []map[string]any{{
				"id": 4711, "message": "{file}",
				"messageParameters": map[string]any{"file": map[string]any{
					"type": "file", "name": "note.txt", "path": "Talk/note.txt", "mimetype": "text/plain",
				}},
			}})
		case strings.HasPrefix(r.URL.Path, "/remote.php/dav/"):
			mu.Lock()
			davPaths = append(davPaths, r.URL.Path)
			mu.Unlock()
			_, _ = w.Write([]byte("x"))
		default:
			writeOCS(t, w, map[string]any{})
		}
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	_, err := client.convertMessage(context.Background(),
		newTestPortal(client.host(), "abc123token"), &stubUploader{}, incomingFileMessage())
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	if len(davPaths) != 1 {
		t.Fatalf("made %d WebDAV requests, want 1", len(davPaths))
	}
	if want := "/remote.php/dav/files/alice/Talk/note.txt"; davPaths[0] != want {
		t.Errorf("downloaded %s, want the path resolved for this login (%s)", davPaths[0], want)
	}
}

func TestConvertMessagePicksMsgTypeFromMime(t *testing.T) {
	for mimeType, want := range map[string]event.MessageType{
		"image/png": event.MsgImage,
		"video/mp4": event.MsgVideo,
		"audio/ogg": event.MsgAudio,
	} {
		t.Run(mimeType, func(t *testing.T) {
			serverURL := fileServer(t, map[string]any{
				"type": "file", "name": "thing", "path": "Talk/thing", "mimetype": mimeType,
			}, "data")
			client := newTestClient(t, serverURL, "alice", Config{})

			converted, err := client.convertMessage(context.Background(),
				newTestPortal(client.host(), "abc123token"), &stubUploader{}, incomingFileMessage())
			if err != nil {
				t.Fatalf("convertMessage: %v", err)
			}
			if got := converted.Parts[0].Content.MsgType; got != want {
				t.Errorf("MsgType = %q, want %q", got, want)
			}
		})
	}
}

// Talk carries a caption on the share, which Matrix renders as a body distinct
// from the file name.
func TestConvertMessageCarriesCaption(t *testing.T) {
	serverURL := fileServer(t, map[string]any{
		"type": "file", "name": "cat.png", "path": "Talk/cat.png", "mimetype": "image/png",
	}, "img")
	client := newTestClient(t, serverURL, "alice", Config{})

	msg := incomingFileMessage()
	msg.Text = "look at this {file}"

	converted, err := client.convertMessage(context.Background(),
		newTestPortal(client.host(), "abc123token"), &stubUploader{}, msg)
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	content := converted.Parts[0].Content
	if content.Body != "look at this" {
		t.Errorf("Body = %q, want the caption", content.Body)
	}
	if content.FileName != "cat.png" {
		t.Errorf("FileName = %q, want the file name", content.FileName)
	}
}

// A file that cannot be moved must not take the message down with it: the text
// rendering still names the file, which beats the message never arriving.
func TestConvertMessageFallsBackWhenFileFails(t *testing.T) {
	serverURL, _ := newOCSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/remote.php/dav/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeOCS(t, w, []map[string]any{{
			"id": 4711, "message": "{file}",
			"messageParameters": map[string]any{"file": map[string]any{
				"type": "file", "name": "note.txt", "path": "Talk/note.txt", "mimetype": "text/plain",
			}},
		}})
	})
	client := newTestClient(t, serverURL, "alice", Config{})

	converted, err := client.convertMessage(context.Background(),
		newTestPortal(client.host(), "abc123token"), &stubUploader{}, incomingFileMessage())
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	content := converted.Parts[0].Content
	if content.MsgType != event.MsgText {
		t.Errorf("MsgType = %q, want a text fallback", content.MsgType)
	}
	if content.Body != "note.txt" {
		t.Errorf("Body = %q, want the file name as text", content.Body)
	}
}

// System messages never carry a file worth downloading, and a message being
// converted without an intent has nowhere to upload to.
func TestConvertFilePartSkipped(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})
	portal := newTestPortal(client.host(), "abc123token")

	system := incomingFileMessage()
	system.SystemType = "file_shared"
	if part := client.convertFilePart(context.Background(), portal, &stubUploader{}, system); part != nil {
		t.Error("a system message should not be treated as a file share")
	}
	if part := client.convertFilePart(context.Background(), portal, nil, incomingFileMessage()); part != nil {
		t.Error("without an intent there is nowhere to upload the file")
	}
}

// newMediaMessage builds the outgoing Matrix event for a file.
func newMediaMessage(client *NCTalkClient, msgType event.MessageType, body, fileName string) *bridgev2.MatrixMessage {
	return newTestMatrixMessage(
		newTestPortal(client.host(), "abc123"),
		&event.MessageEventContent{
			MsgType:  msgType,
			Body:     body,
			FileName: fileName,
			URL:      "mxc://matrix.example.com/abc",
			Info:     &event.FileInfo{MimeType: "image/png"},
		},
	)
}

// shareRecorder is a Nextcloud stand-in for the outgoing file path. It answers
// the folder, collision, upload and share calls, and keeps the share's form
// body — which the shared OCS recorder cannot, since the share is not the last
// request an outgoing file makes.
type shareRecorder struct {
	url string

	mu        sync.Mutex
	shareForm url.Values
	methods   []string
	putPath   string
}

func (s *shareRecorder) form() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shareForm
}

func (s *shareRecorder) sawMethods() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.methods, ",")
}

// newShareRecorder starts the server. recent is what the reference lookup over
// recent messages returns.
func newShareRecorder(t *testing.T, recent func() []map[string]any) *shareRecorder {
	t.Helper()
	rec := &shareRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		rec.mu.Lock()
		rec.methods = append(rec.methods, r.Method)
		if r.Method == http.MethodPut {
			rec.putPath = r.URL.Path
		}
		rec.mu.Unlock()

		switch {
		case r.Method == "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound) // nothing in the way
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "files_sharing"):
			form, err := url.ParseQuery(string(body))
			if err != nil {
				t.Errorf("parse share body %q: %v", body, err)
			}
			rec.mu.Lock()
			rec.shareForm = form
			rec.mu.Unlock()
			writeOCS(t, w, map[string]any{"id": "7"})
		default:
			writeOCS(t, w, recent())
		}
	}))
	t.Cleanup(srv.Close)
	rec.url = srv.URL
	return rec
}

func TestHandleMatrixMessageSendsFile(t *testing.T) {
	// The reference is derived from the Matrix event ID, so it is the same for
	// every message built by the helper.
	ref := referenceID(newTestMatrixMessage(nil, nil).Event.ID)
	rec := newShareRecorder(t, func() []map[string]any {
		return []map[string]any{{
			"id": 4711, "token": "abc123", "timestamp": 1700000000, "referenceId": ref,
		}}
	})
	client := newTestClient(t, rec.url, "alice", Config{})
	client.downloader = &stubDownloader{data: []byte("png bytes")}

	resp, err := client.HandleMatrixMessage(context.Background(),
		newMediaMessage(client, event.MsgImage, "cat.png", ""))
	if err != nil {
		t.Fatalf("HandleMatrixMessage: %v", err)
	}
	// The Talk message ID has to come back, or the file echoes into the room a
	// second time when the webhook delivers it.
	if want := makeMessageID(client.host(), "abc123", 4711); resp.DB.ID != want {
		t.Errorf("DB.ID = %q, want %q", resp.DB.ID, want)
	}
	if resp.StreamOrder != 4711 {
		t.Errorf("StreamOrder = %d, want the Talk message ID", resp.StreamOrder)
	}
	for _, want := range []string{"MKCOL", "HEAD", "PUT", "POST"} {
		if !strings.Contains(rec.sawMethods(), want) {
			t.Errorf("no %s request was made; got %s", want, rec.sawMethods())
		}
	}
	if want := "/remote.php/dav/files/alice/Talk/cat.png"; rec.putPath != want {
		t.Errorf("uploaded to %s, want %s", rec.putPath, want)
	}
	if got := rec.form().Get("referenceId"); got != ref {
		t.Errorf("referenceId = %q, want %q", got, ref)
	}
}

func TestSendFileUsesCaptionAndFileName(t *testing.T) {
	rec := newShareRecorder(t, func() []map[string]any { return nil })
	client := newTestClient(t, rec.url, "alice", Config{})
	client.downloader = &stubDownloader{data: []byte("png bytes")}

	// A Matrix caption is the body, while the file name lives in FileName.
	msg := newMediaMessage(client, event.MsgImage, "look at this", "cat.png")
	if _, err := client.HandleMatrixMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMatrixMessage: %v", err)
	}

	if got := rec.form().Get("path"); got != "/Talk/cat.png" {
		t.Errorf("shared path = %q, want the file name", got)
	}
	var meta struct {
		Caption string `json:"caption"`
	}
	raw := rec.form().Get("talkMetaData")
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("talkMetaData = %q: %v", raw, err)
	}
	if meta.Caption != "look at this" {
		t.Errorf("caption = %q", meta.Caption)
	}
}

// Without a file name the body is the name, and there is no caption to send.
func TestSendFileWithoutCaption(t *testing.T) {
	rec := newShareRecorder(t, func() []map[string]any { return nil })
	client := newTestClient(t, rec.url, "alice", Config{})
	client.downloader = &stubDownloader{data: []byte("bytes")}

	msg := newMediaMessage(client, event.MsgFile, "report.pdf", "")
	if _, err := client.HandleMatrixMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMatrixMessage: %v", err)
	}
	if got := rec.form().Get("path"); got != "/Talk/report.pdf" {
		t.Errorf("shared path = %q", got)
	}
	if got := rec.form().Get("talkMetaData"); got != "" {
		t.Errorf("talkMetaData = %q, want none", got)
	}
}

// The file did reach Talk; only its ID is unknown. Failing the message would be
// wrong, so it is recorded under its reference instead.
func TestSendFileWithoutFindingTheMessage(t *testing.T) {
	rec := newShareRecorder(t, func() []map[string]any { return nil })
	client := newTestClient(t, rec.url, "alice", Config{})
	client.downloader = &stubDownloader{data: []byte("bytes")}

	resp, err := client.HandleMatrixMessage(context.Background(),
		newMediaMessage(client, event.MsgFile, "report.pdf", ""))
	if err != nil {
		t.Fatalf("HandleMatrixMessage: %v", err)
	}
	if !isRelayedMessageID(resp.DB.ID) {
		t.Errorf("DB.ID = %q, want an ID derived from the reference", resp.DB.ID)
	}
}

func TestSendFileRejections(t *testing.T) {
	t.Run("attachments disabled", func(t *testing.T) {
		client := newTestClient(t, testServer, "alice", Config{})
		client.caps = &nctalk.Capabilities{Config: map[string]any{
			"attachments": map[string]any{"allowed": false},
		}}
		client.downloader = &stubDownloader{data: []byte("x")}

		_, err := client.HandleMatrixMessage(context.Background(),
			newMediaMessage(client, event.MsgImage, "cat.png", ""))
		if !errors.Is(err, errAttachmentsDisabled) {
			t.Errorf("err = %v, want errAttachmentsDisabled", err)
		}
	})

	t.Run("download failed", func(t *testing.T) {
		client := newTestClient(t, testServer, "alice", Config{})
		client.downloader = &stubDownloader{err: errors.New("gone")}

		_, err := client.HandleMatrixMessage(context.Background(),
			newMediaMessage(client, event.MsgImage, "cat.png", ""))
		if !errors.Is(err, bridgev2.ErrMediaDownloadFailed) {
			t.Errorf("err = %v, want a download failure", err)
		}
	})

	t.Run("relayed sender", func(t *testing.T) {
		client := newTestClient(t, testServer, "alice", Config{RelayUnlinkedUsers: true})
		client.downloader = &stubDownloader{data: []byte("x")}
		msg := newMediaMessage(client, event.MsgImage, "cat.png", "")
		msg.OrigSender = &bridgev2.OrigSender{UserID: "@bob:matrix.example.com"}

		_, err := client.HandleMatrixMessage(context.Background(), msg)
		if !errors.Is(err, errRelayCannotSendFiles) {
			t.Errorf("err = %v, want errRelayCannotSendFiles", err)
		}
	})

	t.Run("no media source", func(t *testing.T) {
		client := newTestClient(t, testServer, "alice", Config{})
		_, err := client.HandleMatrixMessage(context.Background(),
			newMediaMessage(client, event.MsgImage, "cat.png", ""))
		if err == nil {
			t.Error("expected an error with no way to fetch the media")
		}
	})
}

// GetCapabilities must not offer file support the server has switched off, or
// clients will send files that can only be rejected.
func TestGetCapabilitiesFiles(t *testing.T) {
	client := newTestClient(t, testServer, "alice", Config{})
	portal := newTestPortal(client.host(), "abc123")

	client.caps = &nctalk.Capabilities{Config: map[string]any{
		"attachments": map[string]any{"allowed": true},
	}}
	feats := client.GetCapabilities(context.Background(), portal)
	if feats.File == nil {
		t.Fatal("no file features advertised")
	}
	for _, msgType := range []event.MessageType{event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile} {
		ff, ok := feats.File[msgType]
		if !ok {
			t.Errorf("%s is not advertised", msgType)
			continue
		}
		if !ff.GetMimeSupport("application/x-anything").Full() {
			t.Errorf("%s does not accept arbitrary MIME types", msgType)
		}
		if !ff.Caption.Full() {
			t.Errorf("%s does not advertise captions", msgType)
		}
	}

	client.caps = &nctalk.Capabilities{Config: map[string]any{
		"attachments": map[string]any{"allowed": false},
	}}
	if feats := client.GetCapabilities(context.Background(), portal); feats.File != nil {
		t.Error("file support advertised even though the server disallows attachments")
	}
}

func TestOutgoingFileNameAndCaption(t *testing.T) {
	tests := []struct {
		name        string
		content     *event.MessageEventContent
		wantName    string
		wantCaption string
	}{
		{"file name and caption",
			&event.MessageEventContent{Body: "look", FileName: "cat.png"}, "cat.png", "look"},
		{"body is the name",
			&event.MessageEventContent{Body: "cat.png"}, "cat.png", ""},
		{"identical name and body",
			&event.MessageEventContent{Body: "cat.png", FileName: "cat.png"}, "cat.png", ""},
		{"nothing at all",
			&event.MessageEventContent{}, "file", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := outgoingFileName(tc.content); got != tc.wantName {
				t.Errorf("outgoingFileName = %q, want %q", got, tc.wantName)
			}
			if got := outgoingCaption(tc.content); got != tc.wantCaption {
				t.Errorf("outgoingCaption = %q, want %q", got, tc.wantCaption)
			}
		})
	}
}
