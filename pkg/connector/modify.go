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
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

var (
	_ bridgev2.EditHandlingNetworkAPI        = (*NCTalkClient)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI   = (*NCTalkClient)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI = (*NCTalkClient)(nil)
)

// Status errors for changing a message that already exists in Talk. Like the
// send-side ones, these are shaped so Element shows the reason next to the
// event rather than a bare failure.
var (
	errRelayedNotModifiable = bridgev2.WrapErrorInStatus(errors.New("this message was posted by the bridge bot, which Nextcloud Talk does not report an ID for, so it can no longer be changed")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusUnsupported)
	errRelaySenderCannotModify = bridgev2.WrapErrorInStatus(errors.New("you have no linked Nextcloud account, so the bridge cannot change messages on your behalf")).
					WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
					WithErrorReason(event.MessageStatusUnsupported)
	errEditTargetTooOld = bridgev2.WrapErrorInStatus(errors.New("Nextcloud Talk only allows editing a message within 24 hours of sending it")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusTooOld)
	errDeleteTargetTooOld = bridgev2.WrapErrorInStatus(errors.New("Nextcloud Talk only allows deleting a message within 6 hours of sending it")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusTooOld)
	errNotYourMessage = bridgev2.WrapErrorInStatus(errors.New("Nextcloud Talk does not let you change that message")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusUnsupported)
	errTargetMessageGone = bridgev2.WrapErrorInStatus(errors.New("that message no longer exists in Nextcloud Talk")).
				WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
				WithErrorReason(event.MessageStatusUnsupported)
)

// HandleMatrixEdit implements bridgev2.EditHandlingNetworkAPI.
//
// Talk edits a message in place and keeps its ID, so the bridged mapping is
// unaffected and only the edit count needs updating.
func (c *NCTalkClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	if msg.OrigSender != nil {
		return errRelaySenderCannotModify
	}
	token, messageID, err := c.talkTarget(msg.Portal, msg.EditTarget)
	if err != nil {
		return err
	}
	if tooOld(msg.EditTarget.Timestamp, talkEditMaxAge) {
		return errEditTargetTooOld
	}

	text, err := c.renderOutgoingText(ctx, msg.Content)
	if err != nil {
		return err
	}
	if text == "" {
		return errEmptyMessage
	}
	if utf8.RuneCountInString(text) > nctalk.MaxChatLength {
		return errMessageTooLong
	}

	if err := c.Client.EditMessage(ctx, token, messageID, text); err != nil {
		return c.wrapModifyError(ctx, err, "edit")
	}
	msg.EditTarget.EditCount++
	return nil
}

// HandleMatrixMessageRemove implements bridgev2.RedactionHandlingNetworkAPI.
//
// Talk does not remove a deleted message so much as blank it, leaving a
// "message deleted" placeholder where it was. That is what a Talk user sees
// when they delete their own message, so it is also the honest result of
// redacting a bridged one.
func (c *NCTalkClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	if msg.OrigSender != nil {
		return errRelaySenderCannotModify
	}
	token, messageID, err := c.talkTarget(msg.Portal, msg.TargetMessage)
	if err != nil {
		return err
	}
	if tooOld(msg.TargetMessage.Timestamp, talkDeleteMaxAge) {
		return errDeleteTargetTooOld
	}

	if err := c.Client.DeleteMessage(ctx, token, messageID); err != nil {
		return c.wrapModifyError(ctx, err, "delete")
	}
	return nil
}

// HandleMatrixReadReceipt implements bridgev2.ReadReceiptHandlingNetworkAPI.
//
// Talk keeps one read marker per conversation, addressed by message ID, so a
// receipt is only actionable when it points at a message the bridge knows.
func (c *NCTalkClient) HandleMatrixReadReceipt(ctx context.Context, msg *bridgev2.MatrixReadReceipt) error {
	if msg.ExactMessage == nil {
		// Implicit receipts arrive on every outgoing event and carry no message,
		// as do receipts for anything that is not a bridged message.
		return nil
	}
	if !c.capabilities().Has(nctalk.CapChatReadStatus) {
		return nil
	}
	// Receipts are not ordered, and Talk's marker is absolute rather than a
	// high-water mark, so a late one would drag it backwards.
	if !msg.LastRead.IsZero() && !msg.ReadUpTo.After(msg.LastRead) {
		return nil
	}

	token, messageID, err := c.talkTarget(msg.Portal, msg.ExactMessage)
	if err != nil {
		// A read receipt is not worth surfacing to the user over.
		zerolog.Ctx(ctx).Debug().Err(err).
			Str("message_id", string(msg.ExactMessage.ID)).
			Msg("Ignoring read receipt for a message with no Talk equivalent")
		return nil
	}
	return c.Client.SetReadMarker(ctx, token, messageID)
}

// talkTarget resolves a stored message into the conversation token and Talk
// message ID needed to act on it.
//
// Messages the bridge relayed through the bot endpoint are rejected rather than
// guessed at: Talk does not report the ID it assigned them, so there is nothing
// to address.
func (c *NCTalkClient) talkTarget(portal *bridgev2.Portal, target *database.Message) (string, int64, error) {
	host, token, err := parsePortalID(portal.ID)
	if err != nil {
		return "", 0, err
	}
	if host != c.host() {
		return "", 0, fmt.Errorf("portal %s belongs to a different Nextcloud server", portal.ID)
	}
	if isRelayedMessageID(target.ID) {
		return "", 0, errRelayedNotModifiable
	}

	targetHost, targetToken, messageID, err := parseMessageID(target.ID)
	if err != nil {
		return "", 0, err
	}
	if targetHost != host || targetToken != token {
		return "", 0, fmt.Errorf("message %s is not in conversation %s", target.ID, token)
	}
	return token, messageID, nil
}

// tooOld reports whether a message sent at ts has passed one of Talk's windows.
// A zero timestamp means the bridge never learned when the message was sent, in
// which case letting Talk decide beats refusing on a guess.
func tooOld(ts time.Time, window time.Duration) bool {
	return !ts.IsZero() && time.Since(ts) > window
}

// capabilities returns the server's Talk capabilities, falling back to the list
// cached at login time when Connect has not run yet.
func (c *NCTalkClient) capabilities() *nctalk.Capabilities {
	if c.caps != nil {
		return c.caps
	}
	return &nctalk.Capabilities{Features: c.meta().Features}
}

// wrapModifyError turns a Talk rejection of a change to an existing message
// into a status naming what actually went wrong.
func (c *NCTalkClient) wrapModifyError(ctx context.Context, err error, action string) error {
	switch {
	case nctalk.IsNotFound(err):
		return errTargetMessageGone
	case nctalk.IsForbidden(err):
		return errNotYourMessage
	case nctalk.IsBadRequest(err), nctalk.IsMethodNotAllowed(err):
		// Talk answers 400 and 405 for a change it will not make to a message
		// that exists: past the time window, a system message, or one already
		// deleted. It does not say which, so neither can the bridge.
		zerolog.Ctx(ctx).Warn().Err(err).Msgf("Talk refused to %s a message", action)
		return bridgev2.WrapErrorInStatus(fmt.Errorf("Nextcloud Talk would not %s that message", action)).
			WithIsCertain(true).WithErrorAsMessage().WithSendNotice(true).
			WithErrorReason(event.MessageStatusUnsupported)
	default:
		return c.wrapSendError(ctx, err)
	}
}
