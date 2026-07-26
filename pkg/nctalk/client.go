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

// Package nctalk is a client for the Nextcloud OCS APIs used by the Matrix
// bridge: Talk conversations, chat, reactions, bots, and the Login Flow v2
// authentication handshake.
//
// It deliberately has no dependency on mautrix so that it can be exercised
// against a real Nextcloud server in isolation.
package nctalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UserAgent identifies the bridge to Nextcloud. Login Flow v2 uses this string
// as the name of the app password it creates, so it shows up verbatim in the
// user's Security settings.
const UserAgent = "Matrix Bridge (matrix-nctalk)"

// SpreedAPI is the OCS base path for the Talk (spreed) app.
const SpreedAPI = "/ocs/v2.php/apps/spreed"

// Client talks to a single Nextcloud server as a single user.
//
// The zero value is not usable; construct with NewClient.
type Client struct {
	// BaseURL is the server root with no trailing slash, e.g. https://cloud.example.com
	BaseURL string
	// Username is the Nextcloud user ID (the login name, not the display name).
	Username string
	// AppPassword is an app password, normally obtained via Login Flow v2.
	AppPassword string

	HTTP *http.Client
}

// NewClient returns a Client for the given server and credentials.
// baseURL may include a trailing slash; it is normalised away.
func NewClient(baseURL, username, appPassword string) *Client {
	return &Client{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		Username:    username,
		AppPassword: appPassword,
		HTTP:        &http.Client{Timeout: 30 * time.Second},
	}
}

// Host returns the hostname of the server, used as the prefix for bridge-side
// network IDs so that one bridge can serve several Nextcloud servers.
func (c *Client) Host() string {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return c.BaseURL
	}
	return u.Host
}

// ocsEnvelope is the wrapper every OCS response is nested inside.
type ocsEnvelope struct {
	OCS struct {
		Meta struct {
			Status     string `json:"status"`
			StatusCode int    `json:"statuscode"`
			Message    string `json:"message"`
		} `json:"meta"`
		Data json.RawMessage `json:"data"`
	} `json:"ocs"`
}

// Error is a failure reported inside an OCS envelope, or a non-2xx HTTP status
// from an endpoint that does not use the envelope.
type Error struct {
	// StatusCode is the OCS meta statuscode where available, else the HTTP status.
	StatusCode int
	// HTTPStatus is the transport-level status code.
	HTTPStatus int
	Message    string
	// Body is the raw response, retained for diagnostics on unparseable errors.
	Body string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("nextcloud: %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("nextcloud: %d (http %d)", e.StatusCode, e.HTTPStatus)
}

// IsNotFound reports whether err is an OCS 404. Talk uses 404 both for missing
// conversations and for "nothing new" on some polling endpoints, so callers
// often need to distinguish it.
func IsNotFound(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.StatusCode == http.StatusNotFound
}

// IsUnauthorized reports whether err indicates the app password is no longer
// valid, which the bridge surfaces as a bad-credentials bridge state.
func IsUnauthorized(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.StatusCode == http.StatusUnauthorized || e.HTTPStatus == http.StatusUnauthorized
}

// IsNotModified reports whether err is a 304. Talk answers the chat endpoint
// with one, and no body at all, whenever there is nothing on the requested side
// of the cursor — which for history paging means the end has been reached.
func IsNotModified(err error) bool {
	return hasStatus(err, http.StatusNotModified)
}

// Response carries the decoded payload plus headers the Talk API uses to
// convey chat cursors (X-Chat-Last-Given and friends).
type Response struct {
	Data    json.RawMessage
	Headers http.Header
}

// request performs an authenticated OCS call and unwraps the envelope.
//
// query is appended to the URL; form, when non-nil, is sent as a urlencoded
// body. Talk's write endpoints accept form encoding rather than JSON.
func (c *Client) request(ctx context.Context, method, path string, query, form url.Values) (*Response, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if c.Username != "" || c.AppPassword != "" {
		req.SetBasicAuth(c.Username, c.AppPassword)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// A 401 never carries a useful envelope; report it directly so callers get
	// a clean IsUnauthorized signal rather than a JSON parse failure.
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &Error{StatusCode: http.StatusUnauthorized, HTTPStatus: resp.StatusCode, Message: "invalid credentials", Body: truncate(raw)}
	}

	// Nextcloud skips the envelope entirely on some replies: 304 for "nothing to
	// return" on the chat endpoints, and 400 when a query parameter is outside
	// the range the route declares. Reporting the status is far more use than
	// complaining that an empty body is not JSON.
	if len(bytes.TrimSpace(raw)) == 0 {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return &Response{Headers: resp.Header}, nil
		}
		return nil, &Error{
			StatusCode: resp.StatusCode,
			HTTPStatus: resp.StatusCode,
			Message:    fmt.Sprintf("empty response body (%s)", resp.Status),
		}
	}

	var env ocsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &Error{
			StatusCode: resp.StatusCode,
			HTTPStatus: resp.StatusCode,
			Message:    "unparseable OCS response",
			Body:       truncate(raw),
		}
	}

	if env.OCS.Meta.StatusCode < 200 || env.OCS.Meta.StatusCode >= 300 {
		return nil, &Error{
			StatusCode: env.OCS.Meta.StatusCode,
			HTTPStatus: resp.StatusCode,
			Message:    env.OCS.Meta.Message,
			Body:       truncate(raw),
		}
	}

	return &Response{Data: env.OCS.Data, Headers: resp.Header}, nil
}

// requestJSON performs an OCS call and unmarshals the data payload into out.
// out may be nil to discard the payload.
func (c *Client) requestJSON(ctx context.Context, method, path string, query, form url.Values, out any) (*Response, error) {
	resp, err := c.request(ctx, method, path, query, form)
	if err != nil {
		return nil, err
	}
	if out != nil && len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return nil, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return resp, nil
}

// postJSONNoAuth posts a JSON body without OCS auth and decodes a plain (non-
// envelope) JSON response. Login Flow v2 endpoints are not OCS endpoints.
func (c *Client) postJSONNoAuth(ctx context.Context, rawURL string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{StatusCode: resp.StatusCode, HTTPStatus: resp.StatusCode, Body: truncate(raw)}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func truncate(b []byte) string {
	const max = 512
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
