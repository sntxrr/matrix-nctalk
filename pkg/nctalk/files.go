package nctalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// DefaultAttachmentFolder is where Talk puts shared files when the server does
// not say otherwise.
const DefaultAttachmentFolder = "/Talk"

// shareTypeRoom is Nextcloud's share type for a Talk conversation, from
// IShare::TYPE_ROOM in spreed. Sharing a file with this type is what makes it
// appear as a chat message rather than only in the recipient's Files app.
const shareTypeRoom = 10

// AttachmentFolder returns the folder Talk uploads attachments into, which is
// configurable per server and reported in the Talk capabilities.
func (c *Capabilities) AttachmentFolder() string {
	attachments, ok := c.Config["attachments"].(map[string]any)
	if !ok {
		return DefaultAttachmentFolder
	}
	folder, ok := attachments["folder"].(string)
	if !ok || folder == "" {
		return DefaultAttachmentFolder
	}
	return folder
}

// AttachmentsAllowed reports whether the server permits sharing files into
// conversations at all.
func (c *Capabilities) AttachmentsAllowed() bool {
	attachments, ok := c.Config["attachments"].(map[string]any)
	if !ok {
		// Older servers do not advertise the setting and do allow attachments.
		return true
	}
	allowed, ok := attachments["allowed"].(bool)
	return !ok || allowed
}

// davPath builds the WebDAV URL for a path in the authenticated user's files.
//
// Each segment is escaped separately so that the slashes separating them
// survive while everything else — spaces, hashes, question marks — does not
// change the meaning of the URL.
func (c *Client) davPath(filePath string) string {
	var b strings.Builder
	b.WriteString(c.BaseURL)
	b.WriteString("/remote.php/dav/files/")
	b.WriteString(url.PathEscape(c.Username))
	for _, segment := range strings.Split(strings.Trim(filePath, "/"), "/") {
		if segment == "" {
			continue
		}
		b.WriteByte('/')
		b.WriteString(url.PathEscape(segment))
	}
	return b.String()
}

// dav issues an authenticated WebDAV request and returns the response, which
// the caller must close.
func (c *Client) dav(ctx context.Context, method, filePath string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.davPath(filePath), body)
	if err != nil {
		return nil, fmt.Errorf("build WebDAV request: %w", err)
	}
	req.SetBasicAuth(c.Username, c.AppPassword)
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, filePath, err)
	}
	return resp, nil
}

// davStatus issues a WebDAV request that returns no body and reports its status.
func (c *Client) davStatus(ctx context.Context, method, filePath string, body io.Reader) (int, error) {
	resp, err := c.dav(ctx, method, filePath, body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// EnsureFolder creates a folder if it is not already there.
//
// WebDAV answers 405 when the collection exists, which is success for this
// purpose rather than an error.
func (c *Client) EnsureFolder(ctx context.Context, folder string) error {
	status, err := c.davStatus(ctx, "MKCOL", folder, nil)
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status < 300, status == http.StatusMethodNotAllowed:
		return nil
	default:
		return fmt.Errorf("create folder %s: unexpected status %d", folder, status)
	}
}

// FileExists reports whether a path is already taken.
func (c *Client) FileExists(ctx context.Context, filePath string) (bool, error) {
	status, err := c.davStatus(ctx, http.MethodHead, filePath, nil)
	if err != nil {
		return false, err
	}
	switch {
	case status >= 200 && status < 300:
		return true, nil
	case status == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("check %s: unexpected status %d", filePath, status)
	}
}

// UploadFile writes a file, overwriting whatever is at the path.
//
// Callers that must not clobber an existing file should pick a free name with
// FreeFilePath first; WebDAV PUT overwrites silently.
func (c *Client) UploadFile(ctx context.Context, filePath string, data []byte, mimeType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.davPath(filePath), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}
	req.SetBasicAuth(c.Username, c.AppPassword)
	req.Header.Set("User-Agent", UserAgent)
	if mimeType != "" {
		req.Header.Set("Content-Type", mimeType)
	}
	req.ContentLength = int64(len(data))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("upload %s: %w", filePath, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{
			StatusCode: resp.StatusCode,
			HTTPStatus: resp.StatusCode,
			Message:    "upload rejected: " + resp.Status,
			Body:       string(raw),
		}
	}
	return nil
}

// DownloadFile reads a file from the authenticated user's files. maxBytes caps
// how much is read so a hostile or merely enormous file cannot exhaust memory.
func (c *Client) DownloadFile(ctx context.Context, filePath string, maxBytes int64) ([]byte, error) {
	resp, err := c.dav(ctx, http.MethodGet, filePath, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &Error{
			StatusCode: resp.StatusCode,
			HTTPStatus: resp.StatusCode,
			Message:    "download rejected: " + resp.Status,
			Body:       string(raw),
		}
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filePath, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s is larger than the %d byte limit", filePath, maxBytes)
	}
	return data, nil
}

// FreeFilePath returns a path in folder based on name that nothing occupies
// yet, appending " (2)", " (3)" and so on the way the Nextcloud clients do.
//
// It gives up after a bounded number of attempts rather than looping forever
// against a server that answers every probe the same way.
func (c *Client) FreeFilePath(ctx context.Context, folder, name string) (string, error) {
	const maxAttempts = 50
	base, ext := splitExtension(sanitiseFileName(name))

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		candidate := base + ext
		if attempt > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", base, attempt, ext)
		}
		full := path.Join(folder, candidate)
		exists, err := c.FileExists(ctx, full)
		if err != nil {
			return "", err
		}
		if !exists {
			return full, nil
		}
	}
	return "", fmt.Errorf("could not find a free name for %q in %s", name, folder)
}

// sanitiseFileName reduces a name supplied by another network to something safe
// to place in a path: no directory separators, no leading dots, never empty.
func sanitiseFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', 0:
			return '_'
		}
		if r < 0x20 {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	name = strings.TrimLeft(name, ".")
	if name == "" {
		return "file"
	}
	// Nextcloud rejects names past 255 characters; keep the extension attached.
	if len(name) > 200 {
		base, ext := splitExtension(name)
		if len(ext) > 32 {
			ext = ext[:32]
		}
		name = base[:200-len(ext)] + ext
	}
	return name
}

// splitExtension splits a file name into its stem and extension.
func splitExtension(name string) (base, ext string) {
	ext = path.Ext(name)
	return strings.TrimSuffix(name, ext), ext
}

// ShareFileRequest describes sharing an uploaded file into a conversation.
type ShareFileRequest struct {
	// Path is the file's location in the authenticated user's own files.
	Path string
	// Token is the conversation to share into.
	Token string
	// ReferenceID is echoed onto the chat message Talk creates, which is what
	// lets the bridge recognise the message as its own.
	ReferenceID string
	// Caption becomes the message text; without one the message is just the file.
	Caption string
}

// ShareFileToConversation shares a file into a Talk conversation, which is what
// posts it as a chat message.
//
// Talk assigns the message an ID that this call does not report, so a caller
// that needs it must look the message up by its reference afterwards.
func (c *Client) ShareFileToConversation(ctx context.Context, req ShareFileRequest) error {
	form := url.Values{
		"shareType": {fmt.Sprint(shareTypeRoom)},
		"shareWith": {req.Token},
		"path":      {req.Path},
	}
	if req.ReferenceID != "" {
		ref := req.ReferenceID
		if len(ref) > maxReferenceIDLength {
			ref = ref[:maxReferenceIDLength]
		}
		form.Set("referenceId", ref)
	}
	if req.Caption != "" {
		// Talk reads the caption out of this blob and uses it as the message
		// text; there is no plain form field for it. Marshalling a struct with
		// one string field cannot fail, so the error is not reachable.
		meta, _ := json.Marshal(struct {
			Caption string `json:"caption"`
		}{req.Caption})
		form.Set("talkMetaData", string(meta))
	}

	_, err := c.requestJSON(ctx, http.MethodPost,
		"/ocs/v2.php/apps/files_sharing/api/v1/shares", nil, form, nil)
	if err != nil {
		return fmt.Errorf("share %s into %s: %w", req.Path, req.Token, err)
	}
	return nil
}
