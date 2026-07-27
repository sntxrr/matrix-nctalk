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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LoginFlowInit is the response to starting a Login Flow v2 handshake.
type LoginFlowInit struct {
	// Poll carries the credentials for polling the handshake to completion.
	Poll struct {
		Token    string `json:"token"`
		Endpoint string `json:"endpoint"`
	} `json:"poll"`
	// Login is the URL the user must open in a browser to grant access.
	Login string `json:"login"`

	// Server is the base URL the handshake was started against, retained so the
	// endpoints the server chose can be checked against it. Not part of the
	// wire format.
	Server string `json:"-"`
}

// ErrForeignLoginEndpoint means a server pointed the handshake somewhere other
// than itself.
var ErrForeignLoginEndpoint = errors.New("nctalk: login handshake points at a different server")

// checkSameOrigin reports whether a URL the server handed back addresses the
// same place the handshake was started against.
//
// Without this, the poll endpoint is an arbitrary URL of the server's choosing
// that the bridge will then POST to every couple of seconds for the length of
// the login timeout — which turns "I typed a server address" into a sustained
// request generator aimed wherever that server likes.
func checkSameOrigin(base, candidate string) error {
	b, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("%w: unusable server URL %q", ErrForeignLoginEndpoint, base)
	}
	c, err := url.Parse(candidate)
	if err != nil {
		return fmt.Errorf("%w: unusable URL %q", ErrForeignLoginEndpoint, candidate)
	}
	if !strings.EqualFold(b.Scheme, c.Scheme) || !strings.EqualFold(b.Host, c.Host) {
		return fmt.Errorf("%w: expected %s://%s, got %s://%s",
			ErrForeignLoginEndpoint, b.Scheme, b.Host, c.Scheme, c.Host)
	}
	return nil
}

// LoginFlowResult is the credential set produced by a completed handshake.
type LoginFlowResult struct {
	Server      string `json:"server"`
	LoginName   string `json:"loginName"`
	AppPassword string `json:"appPassword"`
}

// ErrLoginPending is returned by PollLogin while the user has not yet approved
// the request in their browser.
var ErrLoginPending = errors.New("login not yet approved")

// StartLoginFlow begins a Login Flow v2 handshake against baseURL.
//
// This is the preferred way to authenticate: the user approves the bridge in
// their browser and Nextcloud mints a scoped app password, so the bridge never
// handles the account password.
func StartLoginFlow(ctx context.Context, httpClient *http.Client, baseURL string) (*LoginFlowInit, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	c := &Client{BaseURL: baseURL, HTTP: httpClient}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	}

	var out LoginFlowInit
	if err := c.postJSONNoAuth(ctx, baseURL+"/index.php/login/v2", nil, &out); err != nil {
		return nil, fmt.Errorf("start login flow: %w", err)
	}
	if out.Poll.Token == "" || out.Poll.Endpoint == "" || out.Login == "" {
		return nil, fmt.Errorf("start login flow: server returned an incomplete handshake")
	}
	// Both URLs come from the server's own response. The poll endpoint is the
	// one that matters — the bridge calls it repeatedly — but the login URL is
	// shown to the user, so a server that redirected it elsewhere would be
	// phishing through the bridge's own instructions.
	//
	// A Nextcloud whose overwrite.cli.url differs from the address the user
	// typed will trip this. That is the intended behaviour: the alternative is
	// following whatever URL any server names.
	for what, candidate := range map[string]string{"poll endpoint": out.Poll.Endpoint, "login URL": out.Login} {
		if err := checkSameOrigin(baseURL, candidate); err != nil {
			return nil, fmt.Errorf("start login flow: %s: %w", what, err)
		}
	}
	out.Server = baseURL
	return &out, nil
}

// PollLogin checks whether the user has approved the handshake.
//
// Nextcloud answers 404 until approval, which is reported as ErrLoginPending.
func PollLogin(ctx context.Context, httpClient *http.Client, init *LoginFlowInit) (*LoginFlowResult, error) {
	c := &Client{HTTP: httpClient}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	}

	// Re-checked here rather than trusted from StartLoginFlow, so that an init
	// assembled by hand cannot skip it.
	if init.Server != "" {
		if err := checkSameOrigin(init.Server, init.Poll.Endpoint); err != nil {
			return nil, fmt.Errorf("poll login: %w", err)
		}
	}

	form := url.Values{"token": {init.Poll.Token}}
	var out LoginFlowResult
	err := c.postJSONNoAuth(ctx, init.Poll.Endpoint, form, &out)
	if err != nil {
		var ocsErr *Error
		if errors.As(err, &ocsErr) && ocsErr.HTTPStatus == http.StatusNotFound {
			return nil, ErrLoginPending
		}
		return nil, fmt.Errorf("poll login: %w", err)
	}
	if out.AppPassword == "" || out.LoginName == "" {
		return nil, fmt.Errorf("poll login: server returned an incomplete credential set")
	}
	return &out, nil
}

// WaitForLogin polls until the user approves the handshake, ctx is cancelled,
// or the flow expires.
//
// Nextcloud expires a Login Flow v2 handshake after 20 minutes, so callers
// should pass a context bounded by roughly that.
func WaitForLogin(ctx context.Context, httpClient *http.Client, init *LoginFlowInit, interval time.Duration) (*LoginFlowResult, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		res, err := PollLogin(ctx, httpClient, init)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, ErrLoginPending) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// VerifyCredentials confirms the client's credentials work and returns the
// canonical user ID and Talk capabilities.
//
// It doubles as the validation step for the manual app-password login flow.
func (c *Client) VerifyCredentials(ctx context.Context) (*UserDetails, *Capabilities, error) {
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return nil, nil, err
	}
	// The provisioning "user" endpoint resolves the authenticated user without
	// needing to know the canonical casing of their user ID up front.
	var me UserDetails
	if _, err := c.requestJSON(ctx, http.MethodGet, "/ocs/v2.php/cloud/user", nil, nil, &me); err != nil {
		return nil, nil, fmt.Errorf("verify credentials: %w", err)
	}
	if me.ID == "" {
		return nil, nil, fmt.Errorf("verify credentials: server did not return a user ID")
	}
	return &me, caps, nil
}
