package connector

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

// defaultSyncInterval is how often conversations are resynced when the config
// does not say. Talk pushes messages and membership changes over the webhook, so
// this only has to cover what the webhook cannot: anything that happened while
// the bridge was down, and the metadata Talk sends no event for at all.
const defaultSyncInterval = 1 * time.Hour

// portalLookup finds an existing portal without creating one. The bridge
// implements it; tests substitute a stub.
type portalLookup interface {
	GetExistingPortalByKey(ctx context.Context, key networkid.PortalKey) (*bridgev2.Portal, error)
}

// portals returns where to look up existing portals.
func (c *NCTalkClient) portals() portalLookup {
	if c.portalFinder != nil {
		return c.portalFinder
	}
	return c.Main.Bridge
}

// syncInterval returns how often to resync conversations. A negative value
// disables the loop.
func (nc *NCTalkConnector) syncInterval() time.Duration {
	if nc.Config.SyncInterval == 0 {
		return defaultSyncInterval
	}
	return nc.Config.SyncInterval
}

// startPeriodicSync begins resyncing this login's conversations in the
// background. It is safe to call again on reconnect; the previous loop stops.
func (c *NCTalkClient) startPeriodicSync() {
	interval := c.Main.syncInterval()

	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if c.syncCancel != nil {
		c.syncCancel()
		c.syncCancel = nil
	}
	if interval <= 0 {
		return
	}

	// Connect's context is scoped to the connection attempt, so the loop gets one
	// of its own that lives until the login disconnects.
	ctx, cancel := context.WithCancel(context.Background())
	ctx = c.UserLogin.Log.With().Str("component", "conversation sync").Logger().WithContext(ctx)
	c.syncCancel = cancel
	go c.runPeriodicSync(ctx, interval)
}

// stopPeriodicSync ends the background resync loop.
func (c *NCTalkClient) stopPeriodicSync() {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if c.syncCancel != nil {
		c.syncCancel()
		c.syncCancel = nil
	}
}

func (c *NCTalkClient) runPeriodicSync(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	zerolog.Ctx(ctx).Info().
		Stringer("interval", interval).
		Msg("Started resyncing Talk conversations periodically")

	for {
		// The first pass runs immediately rather than after a full interval:
		// Talk retries no webhook, so a message sent while the bridge was down
		// is only recoverable by asking for it, and this is what asks.
		c.syncConversations(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// syncConversations resyncs every conversation this login owns a portal for.
//
// The resync carries the conversation's last activity time, which is what lets
// bridgev2 decide whether Talk holds anything newer than the last bridged
// message and pull it in through backfill.
func (c *NCTalkClient) syncConversations(ctx context.Context) {
	log := zerolog.Ctx(ctx)

	convs, err := c.Client.ListConversations(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Could not list conversations to resync")
		return
	}

	host := c.host()
	var resynced int
	for i := range convs {
		if ctx.Err() != nil {
			return
		}
		conv := &convs[i]
		if !conv.Bridgeable() {
			continue
		}
		key := makePortalKey(host, conv.Token)

		portal, err := c.portals().GetExistingPortalByKey(ctx, key)
		if err != nil {
			log.Err(err).Str("token", conv.Token).Msg("Could not look up the portal for a conversation")
			continue
		}
		// Only conversations somebody has already bridged are resynced. A timer
		// is not a reason to pull every conversation on the server into Matrix.
		if portal == nil || portal.MXID == "" {
			continue
		}
		// When several logins are in one conversation they all see it here, so
		// the one that owns the portal does the work and the room is not
		// resynced once per member. Without a router — the webhook never
		// mounted — there is no owner to defer to, so this login takes it.
		if c.Main.router != nil {
			if owner, err := c.Main.router.Resolve(ctx, host, conv.Token); err != nil || owner == nil || owner.ID != c.UserLogin.ID {
				continue
			}
		}

		c.events().QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatResync,
				PortalKey: key,
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("talk_token", conv.Token).Str("sync_reason", "periodic")
				},
			},
			GetChatInfoFunc: c.GetChatInfo,
			LatestMessageTS: time.Unix(conv.LastActivity, 0),
		})
		resynced++
	}

	log.Debug().
		Int("conversations", len(convs)).
		Int("resynced", resynced).
		Msg("Finished a periodic conversation resync")
}
