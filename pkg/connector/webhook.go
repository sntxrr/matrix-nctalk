package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// webhookPath is the route Talk posts bot events to. The URL given to
// `occ talk:bot:install` is <public address>/_nctalk/webhook.
const webhookPath = "/_nctalk"

// maxWebhookBody bounds how much the handler will read. Talk caps chat messages
// at 32000 characters; 1 MiB leaves ample room for rich object parameters.
const maxWebhookBody = 1 << 20

// webhookWorkers is how many events are processed concurrently. Ordering does
// not depend on this: events carry the Talk message ID as their stream order.
const webhookWorkers = 8

// webhookQueueSize bounds memory if Nextcloud bursts faster than the bridge can
// process. Talk retries nothing, so an overflow drops events; that is preferable
// to unbounded growth, and it is logged loudly.
const webhookQueueSize = 1024

// pendingEvent is a verified webhook event awaiting processing.
type pendingEvent struct {
	evt *nctalk.WebhookEvent
	// host is the Nextcloud hostname the event came from.
	host string
	// receivedAt is used as the event timestamp when Talk sends none.
	receivedAt time.Time
}

// registerWebhook mounts the Talk bot webhook receiver and starts the workers
// that process verified events.
func (nc *NCTalkConnector) registerWebhook(ctx context.Context) error {
	server, ok := nc.Bridge.Matrix.(bridgev2.MatrixConnectorWithServer)
	if !ok || server.GetRouter() == nil {
		return fmt.Errorf("the Matrix connector does not expose an HTTP server, which is required to receive Talk bot webhooks")
	}

	nc.router = newLoginRouter(nc)
	nc.queue = make(chan *pendingEvent, webhookQueueSize)
	for range webhookWorkers {
		go nc.processEvents()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", nc.handleWebhook)
	server.GetRouter().Handle(webhookPath+"/", http.StripPrefix(webhookPath, mux))

	nc.Bridge.Log.Info().
		Str("path", webhookPath+"/webhook").
		Msg("Listening for Nextcloud Talk bot webhooks")
	return nil
}

// handleWebhook receives a Talk bot event.
//
// Talk allows five seconds for a response and counts anything other than 200 or
// 202 as an error, disabling the bot after repeated failures. So this verifies
// the signature, enqueues, and acknowledges — it never performs Matrix or OCS
// work inline.
func (nc *NCTalkConnector) handleWebhook(w http.ResponseWriter, r *http.Request) {
	log := nc.Bridge.Log.With().Str("component", "talk webhook").Logger()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		log.Warn().Err(err).Msg("Failed to read webhook body")
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}

	random := r.Header.Get(nctalk.HeaderRandom)
	signature := r.Header.Get(nctalk.HeaderSignature)
	backend := r.Header.Get(nctalk.HeaderBackend)

	// The signature covers the exact bytes received, so this must happen before
	// any decoding.
	if err := nctalk.VerifyWebhook(random, signature, nc.Config.BotSecret, body); err != nil {
		log.Warn().Err(err).
			Str("backend", backend).
			Msg("Rejected webhook with invalid signature")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	host, err := backendHost(backend)
	if err != nil {
		log.Warn().Err(err).Str("backend", backend).Msg("Rejected webhook with unusable backend header")
		http.Error(w, "invalid backend", http.StatusBadRequest)
		return
	}

	evt, err := nctalk.ParseWebhookEvent(body)
	if err != nil {
		// The signature was valid, so this is a payload shape the bridge does
		// not understand rather than an attack. Acknowledge it so Talk does not
		// count it against the bot, and log it for diagnosis.
		log.Warn().Err(err).Str("host", host).Msg("Could not parse verified webhook payload")
		w.WriteHeader(http.StatusOK)
		return
	}

	pending := &pendingEvent{evt: evt, host: host, receivedAt: time.Now()}
	select {
	case nc.queue <- pending:
		w.WriteHeader(http.StatusOK)
	default:
		// Acknowledge anyway: returning an error would count towards the error
		// budget that disables the bot, and Talk does not retry regardless.
		log.Error().
			Str("host", host).
			Str("activity", evt.Type).
			Int("queue_size", webhookQueueSize).
			Msg("Webhook queue is full, dropping event")
		w.WriteHeader(http.StatusOK)
	}
}

// backendHost extracts the hostname from the X-Nextcloud-Talk-Backend header,
// which Talk sends as a base URL with a trailing slash.
func backendHost(backend string) (string, error) {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		return "", fmt.Errorf("no %s header", nctalk.HeaderBackend)
	}
	u, err := url.Parse(strings.TrimRight(backend, "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("could not parse backend URL %q", backend)
	}
	return u.Host, nil
}

// processEvents drains the webhook queue.
func (nc *NCTalkConnector) processEvents() {
	for pending := range nc.queue {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		log := nc.Bridge.Log.With().
			Str("component", "talk webhook").
			Str("host", pending.host).
			Str("activity", pending.evt.Type).
			Logger()
		ctx = log.WithContext(ctx)

		if err := nc.processEvent(ctx, pending); err != nil {
			log.Err(err).Msg("Failed to process Talk webhook event")
		}
		cancel()
	}
}

// processEvent routes one verified event to its handler.
func (nc *NCTalkConnector) processEvent(ctx context.Context, pending *pendingEvent) error {
	evt := pending.evt

	token, err := evt.Token()
	if err != nil {
		return err
	}

	login, err := nc.router.Resolve(ctx, pending.host, token)
	if err != nil {
		return fmt.Errorf("resolve login for conversation %s: %w", token, err)
	}
	if login == nil {
		zerolog.Ctx(ctx).Debug().
			Str("token", token).
			Msg("No bridge user participates in this conversation, ignoring")
		return nil
	}
	client, ok := login.Client.(*NCTalkClient)
	if !ok {
		return fmt.Errorf("login %s has no Nextcloud client", login.ID)
	}

	switch evt.Type {
	case nctalk.ActivityCreate:
		return client.handleCreate(ctx, evt, token, pending.receivedAt)
	case nctalk.ActivityLike:
		return client.handleReaction(ctx, evt, token, pending.receivedAt, true)
	case nctalk.ActivityUndo:
		return client.handleUndo(ctx, evt, token, pending.receivedAt)
	case nctalk.ActivityJoin:
		return client.handleBotJoin(ctx, token)
	case nctalk.ActivityLeave:
		return client.handleBotLeave(ctx, token)
	case nctalk.ActivityActivity:
		// System messages (calls, membership changes). Handled in M4; the
		// membership half is covered by the chat resync below.
		zerolog.Ctx(ctx).Debug().Msg("Ignoring system message activity")
		return nil
	default:
		zerolog.Ctx(ctx).Debug().Msg("Ignoring unknown activity type")
		return nil
	}
}

// handleCreate bridges a new Talk chat message to Matrix.
func (c *NCTalkClient) handleCreate(ctx context.Context, evt *nctalk.WebhookEvent, token string, receivedAt time.Time) error {
	note, err := evt.Note()
	if err != nil {
		return err
	}
	messageID, err := note.MessageID()
	if err != nil {
		return err
	}
	actorType, actorID, err := parseTalkActor(evt.Actor.ID)
	if err != nil {
		return err
	}
	if !isBridgeableActor(actorType) {
		zerolog.Ctx(ctx).Debug().Str("actor_type", actorType).Msg("Ignoring message from unbridgeable actor")
		return nil
	}

	content, err := note.DecodeContent()
	if err != nil {
		return err
	}

	msg := &talkMessage{
		Token:       token,
		MessageID:   messageID,
		ActorType:   actorType,
		ActorID:     actorID,
		ActorName:   evt.Actor.Name,
		Text:        content.Message,
		Parameters:  content.Parameters,
		IsMarkdown:  note.IsMarkdown(),
		SystemType:  note.Name,
		ReceivedAt:  receivedAt,
		PublishedAt: evt.Timestamp(),
	}
	if note.InReplyTo != nil {
		if parentID, err := note.InReplyTo.Object.MessageID(); err == nil {
			msg.ReplyToID = parentID
		}
	}

	c.queueMessage(ctx, msg)
	return nil
}

// queueMessage hands a converted Talk message to the bridge.
func (c *NCTalkClient) queueMessage(ctx context.Context, msg *talkMessage) {
	c.UserLogin.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*talkMessage]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			PortalKey:    makePortalKey(c.host(), msg.Token),
			CreatePortal: true,
			Sender:       c.eventSender(msg.ActorType, msg.ActorID),
			Timestamp:    msg.timestamp(),
			// Talk message IDs are a per-server monotonic sequence, so they are
			// a correct stream order even though Talk delivers webhooks
			// asynchronously and therefore out of order.
			StreamOrder: msg.MessageID,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Int64("talk_message_id", msg.MessageID).Str("talk_token", msg.Token)
			},
		},
		ID:                 makeMessageID(c.host(), msg.Token, msg.MessageID),
		ConvertMessageFunc: c.convertMessage,
	})
}

// handleReaction bridges a reaction added in Talk.
func (c *NCTalkClient) handleReaction(ctx context.Context, evt *nctalk.WebhookEvent, token string, receivedAt time.Time, _ bool) error {
	note, err := evt.Note()
	if err != nil {
		return err
	}
	targetID, err := note.MessageID()
	if err != nil {
		return err
	}
	actorType, actorID, err := parseTalkActor(evt.Actor.ID)
	if err != nil {
		return err
	}
	if !isBridgeableActor(actorType) {
		return nil
	}
	if evt.Content == "" {
		return fmt.Errorf("reaction activity has no emoji")
	}

	ts := evt.Timestamp()
	if ts.IsZero() {
		ts = receivedAt
	}

	c.UserLogin.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReaction,
			PortalKey: makePortalKey(c.host(), token),
			Sender:    c.eventSender(actorType, actorID),
			Timestamp: ts,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Int64("talk_message_id", targetID).Str("emoji", evt.Content)
			},
		},
		TargetMessage: makeMessageID(c.host(), token, targetID),
		EmojiID:       makeEmojiID(evt.Content),
		Emoji:         evt.Content,
	})
	return nil
}

// handleUndo bridges a reaction removed in Talk. The undone Like is nested
// inside the Undo activity's object.
func (c *NCTalkClient) handleUndo(ctx context.Context, evt *nctalk.WebhookEvent, token string, receivedAt time.Time) error {
	like, err := evt.UndoneLike()
	if err != nil {
		return err
	}
	if like.Type != nctalk.ActivityLike {
		zerolog.Ctx(ctx).Debug().Str("undone_type", like.Type).Msg("Ignoring undo of a non-reaction activity")
		return nil
	}

	note, err := like.Note()
	if err != nil {
		return err
	}
	targetID, err := note.MessageID()
	if err != nil {
		return err
	}
	// The Undo's own actor is who removed the reaction; the nested Like's actor
	// is who originally added it. Talk only lets you remove your own, so they
	// match, but the outer actor is the authoritative one.
	actorType, actorID, err := parseTalkActor(evt.Actor.ID)
	if err != nil {
		return err
	}
	if !isBridgeableActor(actorType) {
		return nil
	}

	emoji := like.Content
	if emoji == "" {
		return fmt.Errorf("undone reaction activity has no emoji")
	}

	ts := evt.Timestamp()
	if ts.IsZero() {
		ts = receivedAt
	}

	c.UserLogin.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReactionRemove,
			PortalKey: makePortalKey(c.host(), token),
			Sender:    c.eventSender(actorType, actorID),
			Timestamp: ts,
		},
		TargetMessage: makeMessageID(c.host(), token, targetID),
		EmojiID:       makeEmojiID(emoji),
		Emoji:         emoji,
	})
	return nil
}

// handleBotJoin reacts to the bridge bot being enabled in a conversation by
// creating or resyncing the portal.
func (c *NCTalkClient) handleBotJoin(ctx context.Context, token string) error {
	c.Main.router.Invalidate(c.host(), token)

	zerolog.Ctx(ctx).Info().Str("token", token).Msg("Bridge bot was enabled in a conversation")
	c.UserLogin.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			PortalKey:    makePortalKey(c.host(), token),
			CreatePortal: true,
		},
	})
	return nil
}

// handleBotLeave reacts to the bridge bot being removed from a conversation.
//
// The portal is kept so history is not lost, but a notice is logged: without
// the bot, no further messages will arrive from that conversation.
func (c *NCTalkClient) handleBotLeave(ctx context.Context, token string) error {
	c.Main.router.Invalidate(c.host(), token)
	zerolog.Ctx(ctx).Warn().
		Str("token", token).
		Msg("Bridge bot was removed from a conversation; no further messages will be received from it")
	return nil
}
