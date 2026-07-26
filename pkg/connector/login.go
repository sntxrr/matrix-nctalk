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
	"fmt"
	"net/url"
	"strings"
	"sync"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

const (
	// LoginFlowIDBrowser uses Nextcloud's Login Flow v2 handshake.
	LoginFlowIDBrowser = "browser"
	// LoginFlowIDAppPassword takes a manually created app password.
	LoginFlowIDAppPassword = "app-password"

	loginStepServerURL   = "com.github.sntxrr.matrix-nextcloud.server_url"
	loginStepBrowserWait = "com.github.sntxrr.matrix-nextcloud.browser_wait"
	loginStepCredentials = "com.github.sntxrr.matrix-nextcloud.credentials"
)

// GetLoginFlows implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Browser",
		Description: "Approve the bridge in your Nextcloud web session. Nextcloud creates a dedicated app password; the bridge never sees your account password.",
		ID:          LoginFlowIDBrowser,
	}, {
		Name:        "App password",
		Description: "Enter a Nextcloud app password created manually under Settings → Security. Useful for headless setups.",
		ID:          LoginFlowIDAppPassword,
	}}
}

// CreateLogin implements bridgev2.NetworkConnector.
func (nc *NCTalkConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case LoginFlowIDBrowser, LoginFlowIDAppPassword:
		return &NCTalkLogin{Main: nc, User: user, FlowID: flowID}, nil
	default:
		return nil, fmt.Errorf("unknown login flow %q", flowID)
	}
}

// NCTalkLogin drives both login flows. Each starts by asking for the server
// URL, then diverges into either a browser handshake or a credential prompt.
type NCTalkLogin struct {
	Main   *NCTalkConnector
	User   *bridgev2.User
	FlowID string

	serverURL string
	flowInit  *nctalk.LoginFlowInit

	// cancelMu guards cancel, which Wait writes and Cancel reads. The bridge
	// may cancel a login from a different goroutine than the one waiting.
	cancelMu sync.Mutex
	cancel   context.CancelFunc
}

var (
	_ bridgev2.LoginProcess               = (*NCTalkLogin)(nil)
	_ bridgev2.LoginProcessUserInput      = (*NCTalkLogin)(nil)
	_ bridgev2.LoginProcessDisplayAndWait = (*NCTalkLogin)(nil)
)

// Start implements bridgev2.LoginProcess.
func (l *NCTalkLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       loginStepServerURL,
		Instructions: "Enter the URL of your Nextcloud server.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:        bridgev2.LoginInputFieldTypeURL,
				ID:          "server_url",
				Name:        "Server URL",
				Description: "For example https://cloud.example.com",
				Validate:    validateServerURL,
			}},
		},
	}, nil
}

// validateServerURL normalises the entered server URL and rejects obvious
// mistakes before any network call is attempted.
func validateServerURL(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("server URL is required")
	}
	// Add the scheme before trimming anything: trimming trailing slashes first
	// would turn a bare "https://" into "https:", which then looks like a
	// schemeless host and gets silently mangled instead of rejected.
	if !strings.Contains(input, "://") {
		input = "https://" + input
	}
	u, err := url.Parse(input)
	if err != nil {
		return "", fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("server URL must be http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("server URL must include a hostname")
	}
	// Keep only what addresses the server. A pasted URL may carry a query or
	// fragment, and those would corrupt every path built from this base.
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

// SubmitUserInput implements bridgev2.LoginProcessUserInput.
func (l *NCTalkLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if serverURL, ok := input["server_url"]; ok && l.serverURL == "" {
		normalised, err := validateServerURL(serverURL)
		if err != nil {
			return nil, err
		}
		l.serverURL = normalised

		host, err := hostOf(l.serverURL)
		if err != nil {
			return nil, err
		}
		if !l.Main.Config.ServerAllowed(host) {
			return nil, fmt.Errorf("this bridge does not allow logins to %s", host)
		}

		if l.FlowID == LoginFlowIDAppPassword {
			return l.credentialsStep(), nil
		}
		return l.startBrowserFlow(ctx)
	}

	username, hasUser := input["username"]
	appPassword, hasPass := input["app_password"]
	if hasUser && hasPass {
		return l.finish(ctx, strings.TrimSpace(username), strings.TrimSpace(appPassword))
	}

	return nil, fmt.Errorf("unexpected login input")
}

func (l *NCTalkLogin) credentialsStep() *bridgev2.LoginStep {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       loginStepCredentials,
		Instructions: "Enter your Nextcloud username and an app password created under Settings → Security → Devices & sessions.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type: bridgev2.LoginInputFieldTypeUsername,
				ID:   "username",
				Name: "Username",
			}, {
				Type: bridgev2.LoginInputFieldTypePassword,
				ID:   "app_password",
				Name: "App password",
			}},
		},
	}
}

// startBrowserFlow opens a Login Flow v2 handshake and asks the user to approve
// it in their browser.
func (l *NCTalkLogin) startBrowserFlow(ctx context.Context) (*bridgev2.LoginStep, error) {
	init, err := nctalk.StartLoginFlow(ctx, l.Main.HTTP, l.serverURL)
	if err != nil {
		return nil, fmt.Errorf("could not start the Nextcloud login handshake: %w", err)
	}
	l.flowInit = init

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeDisplayAndWait,
		StepID:       loginStepBrowserWait,
		Instructions: fmt.Sprintf("Open %s in your browser and approve the request. This page expires in %s.", init.Login, l.Main.loginTimeout()),
		DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{
			Type: bridgev2.LoginDisplayTypeNothing,
			Data: init.Login,
		},
	}, nil
}

// Wait implements bridgev2.LoginProcessDisplayAndWait.
func (l *NCTalkLogin) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	if l.flowInit == nil {
		return nil, fmt.Errorf("login handshake has not been started")
	}

	ctx, cancel := context.WithTimeout(ctx, l.Main.loginTimeout())
	l.cancelMu.Lock()
	l.cancel = cancel
	l.cancelMu.Unlock()
	defer cancel()

	result, err := nctalk.WaitForLogin(ctx, l.Main.HTTP, l.flowInit, 0)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("timed out waiting for approval in the browser")
		}
		return nil, err
	}
	// Nextcloud echoes back the server URL it considers canonical, which may
	// differ from what the user typed (trailing paths, http to https upgrades).
	if result.Server != "" {
		l.serverURL = strings.TrimRight(result.Server, "/")
	}
	return l.finish(ctx, result.LoginName, result.AppPassword)
}

// finish validates the credentials and creates or updates the UserLogin.
func (l *NCTalkLogin) finish(ctx context.Context, username, appPassword string) (*bridgev2.LoginStep, error) {
	if username == "" || appPassword == "" {
		return nil, fmt.Errorf("username and app password are both required")
	}

	client := nctalk.NewClient(l.serverURL, username, appPassword)
	client.HTTP = l.Main.HTTP

	me, caps, err := client.VerifyCredentials(ctx)
	if err != nil {
		if nctalk.IsUnauthorized(err) {
			return nil, fmt.Errorf("Nextcloud rejected those credentials")
		}
		return nil, fmt.Errorf("could not reach Nextcloud: %w", err)
	}
	if !caps.Has(nctalk.CapBotsV1) {
		return nil, fmt.Errorf("this server's Talk version does not support bots; Nextcloud 27.1 with Talk 17.1 or newer is required")
	}

	host, err := hostOf(l.serverURL)
	if err != nil {
		return nil, err
	}

	loginID := makeUserLoginID(host, me.ID)
	ul, err := l.User.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: me.DisplayName,
		RemoteProfile: status.RemoteProfile{
			Name:     me.DisplayName,
			Email:    me.Email,
			Username: me.ID,
		},
		Metadata: &UserLoginMetadata{
			ServerURL:   l.serverURL,
			Username:    me.ID,
			AppPassword: appPassword,
			Features:    caps.Features,
		},
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to save login: %w", err)
	}

	ul.Client.Connect(ul.Log.WithContext(ctx))

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "com.github.sntxrr.matrix-nextcloud.complete",
		Instructions: fmt.Sprintf("Logged in as %s on %s.", me.DisplayName, host),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

// Cancel implements bridgev2.LoginProcess.
func (l *NCTalkLogin) Cancel() {
	l.cancelMu.Lock()
	cancel := l.cancel
	l.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// hostOf extracts the hostname from a server URL.
func hostOf(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("could not determine the hostname of %q", serverURL)
	}
	return u.Host, nil
}
