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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Bot feature bits, mirroring Bot::FEATURE_* in spreed. A bridge bot needs
// webhook (to receive), response (to send) and reaction (to receive reaction
// events), which is what BotFeaturesBridge encodes.
const (
	BotFeatureNone     = 0
	BotFeatureWebhook  = 1
	BotFeatureResponse = 2
	BotFeatureEvent    = 4
	BotFeatureReaction = 8

	BotFeaturesBridge = BotFeatureWebhook | BotFeatureResponse | BotFeatureReaction
)

// Bot install states, mirroring Bot::STATE_* in spreed.
const (
	BotStateDisabled = 0
	BotStateEnabled  = 1
	// BotStateNoSetup means an admin controls which conversations the bot is in
	// and moderators cannot toggle it. EnableBot rejects bots in this state, so
	// the bridge bot must be installed as BotStateEnabled.
	BotStateNoSetup     = 2
	BotStateUnavailable = 3
)

// minRandomLength is the shortest random string spreed's
// ChecksumVerificationService will accept.
const minRandomLength = 32

// randomLength matches what Nextcloud itself generates for bot requests.
const randomLength = 64

// ErrBadSignature indicates a webhook request whose HMAC did not verify.
var ErrBadSignature = errors.New("nctalk: invalid webhook signature")

// sign computes the HMAC that Talk's ChecksumVerificationService expects:
// hex(HMAC-SHA256(random || data, secret)), lowercase.
//
// What goes in data differs by direction, which is the subtle part of this API:
//   - Inbound webhooks sign the raw JSON request body.
//   - Outbound bot calls sign only the message text or the reaction emoji,
//     never the encoded form body.
func sign(random, data, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(random))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhook checks the signature of an incoming Talk webhook request.
//
// body must be the exact bytes received, read before any JSON decoding, since
// the signature covers the raw encoding.
func VerifyWebhook(random, signature, secret string, body []byte) error {
	if len(random) < minRandomLength {
		return fmt.Errorf("%w: random too short (%d)", ErrBadSignature, len(random))
	}
	if signature == "" {
		return fmt.Errorf("%w: no signature header", ErrBadSignature)
	}
	if secret == "" {
		return fmt.Errorf("%w: no secret configured", ErrBadSignature)
	}
	expected := sign(random, string(body), secret)
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(signature))) {
		return ErrBadSignature
	}
	return nil
}

func newRandom() (string, error) {
	buf := make([]byte, randomLength/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// BotClient sends messages and reactions to Talk as a registered bot, using the
// shared secret from `occ talk:bot:install` rather than user credentials.
//
// The bridge uses this only where acting as a specific Nextcloud user is not
// possible: relay mode for Matrix users with no linked account.
type BotClient struct {
	BaseURL string
	Secret  string
	HTTP    *http.Client
}

// NewBotClient returns a BotClient for the given server and shared secret.
func NewBotClient(baseURL, secret string, httpClient *http.Client) *BotClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &BotClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Secret:  secret,
		HTTP:    httpClient,
	}
}

// do issues a signed bot request. signedData is the string covered by the
// signature, which is not the same as the encoded form body.
func (b *BotClient) do(ctx context.Context, method, path, signedData string, form url.Values) error {
	random, err := newRandom()
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, method, b.BaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build bot request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("X-Nextcloud-Talk-Bot-Random", random)
	req.Header.Set("X-Nextcloud-Talk-Bot-Signature", sign(random, signedData, b.Secret))

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{StatusCode: resp.StatusCode, HTTPStatus: resp.StatusCode,
			Message: "bot request rejected: " + resp.Status}
	}
	return nil
}

// SendMessage posts a message to a conversation as the bot.
func (b *BotClient) SendMessage(ctx context.Context, token, message, referenceID string, replyTo int64, silent bool) error {
	form := url.Values{"message": {message}}
	if referenceID != "" {
		form.Set("referenceId", referenceID)
	}
	if replyTo > 0 {
		form.Set("replyTo", strconv.FormatInt(replyTo, 10))
	}
	if silent {
		form.Set("silent", "true")
	}
	// Only the message text is signed, not the form body.
	return b.do(ctx, http.MethodPost,
		SpreedAPI+"/api/v1/bot/"+url.PathEscape(token)+"/message", message, form)
}

// React adds a reaction to a message as the bot.
func (b *BotClient) React(ctx context.Context, token string, messageID int64, emoji string) error {
	form := url.Values{"reaction": {emoji}}
	return b.do(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/bot/%s/reaction/%d", SpreedAPI, url.PathEscape(token), messageID),
		emoji, form)
}

// Unreact removes a reaction from a message as the bot.
func (b *BotClient) Unreact(ctx context.Context, token string, messageID int64, emoji string) error {
	form := url.Values{"reaction": {emoji}}
	return b.do(ctx, http.MethodDelete,
		fmt.Sprintf("%s/api/v1/bot/%s/reaction/%d", SpreedAPI, url.PathEscape(token), messageID),
		emoji, form)
}

// InstalledBot describes a bot as reported by the per-conversation bot list.
type InstalledBot struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	State       int    `json:"state"`
	Features    int    `json:"features"`
	// LastError fields are only present in the admin listing.
	ErrorCount int `json:"error_count"`
}

// ListBots returns the bots available in a conversation, including whether each
// is currently enabled (state).
//
// Requires the authenticated user to be a moderator of the conversation.
func (c *Client) ListBots(ctx context.Context, token string) ([]InstalledBot, error) {
	var out []InstalledBot
	_, err := c.requestJSON(ctx, http.MethodGet,
		SpreedAPI+"/api/v1/bot/"+url.PathEscape(token), nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("list bots in %s: %w", token, err)
	}
	return out, nil
}

// EnableBot enables a bot in a conversation.
//
// Requires moderator rights. Talk rejects this for federated conversations and
// former one-to-one conversations, and for bots not installed in the enabled
// state. Enabling an already-enabled bot succeeds.
func (c *Client) EnableBot(ctx context.Context, token string, botID int64) error {
	_, err := c.requestJSON(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/bot/%s/%d", SpreedAPI, url.PathEscape(token), botID), nil, nil, nil)
	if err != nil {
		return fmt.Errorf("enable bot %d in %s: %w", botID, token, err)
	}
	return nil
}

// DisableBot removes a bot from a conversation. Requires moderator rights.
func (c *Client) DisableBot(ctx context.Context, token string, botID int64) error {
	_, err := c.requestJSON(ctx, http.MethodDelete,
		fmt.Sprintf("%s/api/v1/bot/%s/%d", SpreedAPI, url.PathEscape(token), botID), nil, nil, nil)
	if err != nil {
		return fmt.Errorf("disable bot %d in %s: %w", botID, token, err)
	}
	return nil
}

// FindBotByName locates a bot in a conversation's bot list by display name,
// which is how the bridge discovers its own bot ID without admin access.
func (c *Client) FindBotByName(ctx context.Context, token, name string) (*InstalledBot, error) {
	bots, err := c.ListBots(ctx, token)
	if err != nil {
		return nil, err
	}
	for i := range bots {
		if bots[i].Name == name {
			return &bots[i], nil
		}
	}
	return nil, fmt.Errorf("bot %q not available in conversation %s", name, token)
}
