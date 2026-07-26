package nctalk

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// MaxHistoryLimit is the largest page Talk's chat endpoint will serve.
//
// spreed's controller clamps the value it uses to 200, but the route rejects a
// larger one before that with a 400 and an empty body, so the caller has to
// clamp too.
const MaxHistoryLimit = 200

// HeaderChatLastGiven carries the ID of the last message in a chat response,
// which is the cursor for the next page in the same direction.
const HeaderChatLastGiven = "X-Chat-Last-Given"

// HistoryRequest asks for one page of a conversation's messages.
type HistoryRequest struct {
	Token string
	// LastKnownMessageID is where to read from, exclusive. Zero means start at
	// the newest message, which is only meaningful when reading backwards.
	LastKnownMessageID int64
	// Limit is how many messages to ask for, clamped to MaxHistoryLimit.
	Limit int
	// Future reads messages newer than LastKnownMessageID rather than older.
	Future bool
}

// HistoryPage is one page of a conversation's messages.
type HistoryPage struct {
	// Messages are in the order Talk returned them: newest first when reading
	// backwards, oldest first when reading forwards.
	Messages []Message
	// LastGiven is the ID of the last message of the page, from the
	// X-Chat-Last-Given header, and is the cursor for the next page in the same
	// direction. It is zero when Talk had nothing to return, which is how the
	// end of the history is recognised.
	//
	// It counts messages Talk withheld as well as those it returned, so a page
	// can carry a cursor and no messages, and paging must follow the cursor
	// rather than stopping at the first empty page.
	LastGiven int64
}

// GetHistory reads one page of a conversation's messages.
//
// The read is deliberately invisible on the Talk side: it moves no read marker,
// clears no notifications and does not make the user look online, because it is
// the bridge reading history rather than the user reading their chat.
func (c *Client) GetHistory(ctx context.Context, req HistoryRequest) (*HistoryPage, error) {
	limit := min(max(req.Limit, 1), MaxHistoryLimit)
	lastKnown := max(req.LastKnownMessageID, 0)

	query := url.Values{
		"limit":                   {strconv.Itoa(limit)},
		"lastKnownMessageId":      {strconv.FormatInt(lastKnown, 10)},
		"includeLastKnown":        {"0"},
		"setReadMarker":           {"0"},
		"noStatusUpdate":          {"1"},
		"markNotificationsAsRead": {"0"},
	}
	if req.Future {
		query.Set("lookIntoFuture", "1")
		// Zero timeout: one long poll per conversation is exactly the design the
		// bot webhook exists to avoid, and this endpoint is only used to catch up
		// on what the webhook could not deliver.
		query.Set("timeout", "0")
	} else {
		query.Set("lookIntoFuture", "0")
	}

	var out []Message
	resp, err := c.requestJSON(ctx, http.MethodGet,
		SpreedAPI+"/api/v1/chat/"+url.PathEscape(req.Token), query, nil, &out)
	if err != nil {
		if IsNotModified(err) {
			// Talk's way of saying there is nothing past the cursor.
			return &HistoryPage{}, nil
		}
		return nil, fmt.Errorf("read history of %s: %w", req.Token, err)
	}

	page := &HistoryPage{Messages: out}
	if given := resp.Headers.Get(HeaderChatLastGiven); given != "" {
		page.LastGiven, _ = strconv.ParseInt(given, 10, 64)
	}
	for i := range page.Messages {
		// The chat endpoint reports the token on each message, but a message
		// that has lost it is still addressable, and the caller has it.
		if page.Messages[i].Token == "" {
			page.Messages[i].Token = req.Token
		}
	}
	return page, nil
}
