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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	testUser = "alice"
	testPass = "app-password-value"
)

// recordedRequest captures what the client actually sent, so tests can assert
// on headers, auth and body encoding rather than only on the decoded result.
type recordedRequest struct {
	Method  string
	Path    string
	Query   string
	Header  http.Header
	Body    string
	User    string
	Pass    string
	HasAuth bool
}

// newTestServer starts an HTTP server and returns a Client pointed at it plus a
// pointer to the last request it received.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *recordedRequest) {
	t.Helper()

	last := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		user, pass, ok := r.BasicAuth()
		*last = recordedRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Header:  r.Header.Clone(),
			Body:    string(body),
			User:    user,
			Pass:    pass,
			HasAuth: ok,
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL, testUser, testPass)
	return client, last
}

// writeOCS writes a successful OCS envelope wrapping data.
func writeOCS(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal test data: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ocs":{"meta":{"status":"ok","statuscode":200,"message":"OK"},"data":` + string(raw) + `}}`))
}

// writeOCSError writes an OCS envelope carrying a failure status.
//
// Nextcloud reports OCS-level failures inside the envelope, often with HTTP 200,
// which is why the client cannot rely on the transport status alone.
func writeOCSError(w http.ResponseWriter, httpStatus, ocsStatus int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	body, _ := json.Marshal(map[string]any{
		"ocs": map[string]any{
			"meta": map[string]any{
				"status":     "failure",
				"statuscode": ocsStatus,
				"message":    message,
			},
			"data": nil,
		},
	})
	_, _ = w.Write(body)
}
