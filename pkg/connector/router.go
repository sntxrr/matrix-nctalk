package connector

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/sntxrr/matrix-nextcloud/pkg/nctalk"
)

// loginLister supplies the set of logins the router may choose between.
//
// This is the bridge's cache in production; having it as an interface keeps the
// router testable without standing up a whole Bridge.
type loginLister interface {
	GetAllCachedUserLogins() []*bridgev2.UserLogin
}

// loginRouter resolves an incoming webhook to the UserLogin that should own the
// resulting portal.
//
// The bot webhook is server-wide: Talk delivers one event per conversation
// regardless of how many bridge users are in it. bridgev2, though, needs a
// specific UserLogin to queue the event against. So for each conversation the
// router picks one participating login deterministically and caches it, which
// also keeps the portal's owning login stable across restarts.
type loginRouter struct {
	logins loginLister

	mu sync.RWMutex
	// primary maps "host/token" to the login chosen to own that conversation.
	primary map[string]*bridgev2.UserLogin
	// notParticipant caches logins already known not to be in a conversation,
	// so a busy unbridged conversation does not re-probe on every message.
	notParticipant map[string]map[string]bool
}

func newLoginRouter(logins loginLister) *loginRouter {
	return &loginRouter{
		logins:         logins,
		primary:        make(map[string]*bridgev2.UserLogin),
		notParticipant: make(map[string]map[string]bool),
	}
}

func routeKey(host, token string) string {
	return host + "/" + token
}

// Resolve returns the login that owns the given conversation, probing Nextcloud
// if the answer is not cached.
//
// It returns a nil login with no error when no bridge user participates in the
// conversation, which is a normal condition: the bot may be enabled in
// conversations that nobody has bridged.
func (r *loginRouter) Resolve(ctx context.Context, host, token string) (*bridgev2.UserLogin, error) {
	key := routeKey(host, token)

	r.mu.RLock()
	cached, ok := r.primary[key]
	r.mu.RUnlock()
	if ok && cached != nil && cached.Client != nil {
		return cached, nil
	}

	candidates := r.candidatesForHost(host)
	if len(candidates) == 0 {
		return nil, nil
	}

	// Sort by login ID so the same conversation resolves to the same login on
	// every bridge start, rather than following cache iteration order.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ID < candidates[j].ID
	})

	for _, login := range candidates {
		if r.knownNotParticipant(key, string(login.ID)) {
			continue
		}
		client, ok := login.Client.(*NCTalkClient)
		if !ok {
			continue
		}

		// Fetching the conversation as this user is the cheapest participation
		// test: Talk answers 404 for conversations the user is not in.
		if _, err := client.Client.GetConversation(ctx, token); err != nil {
			if nctalk.IsNotFound(err) {
				r.markNotParticipant(key, string(login.ID))
				continue
			}
			zerolog.Ctx(ctx).Debug().Err(err).
				Str("login_id", string(login.ID)).
				Str("token", token).
				Msg("Could not check conversation participation")
			continue
		}

		r.mu.Lock()
		r.primary[key] = login
		r.mu.Unlock()
		return login, nil
	}

	return nil, nil
}

// candidatesForHost returns all logins belonging to the given Nextcloud host.
func (r *loginRouter) candidatesForHost(host string) []*bridgev2.UserLogin {
	all := r.logins.GetAllCachedUserLogins()
	candidates := make([]*bridgev2.UserLogin, 0, len(all))
	for _, login := range all {
		loginHost, _, err := parseUserLoginID(login.ID)
		if err != nil || !strings.EqualFold(loginHost, host) {
			continue
		}
		if login.Client == nil {
			continue
		}
		candidates = append(candidates, login)
	}
	return candidates
}

func (r *loginRouter) knownNotParticipant(key, loginID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.notParticipant[key][loginID]
}

func (r *loginRouter) markNotParticipant(key, loginID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notParticipant[key] == nil {
		r.notParticipant[key] = make(map[string]bool)
	}
	r.notParticipant[key][loginID] = true
}

// Invalidate drops cached routing for a conversation, so the next event
// re-probes. Called when membership changes.
func (r *loginRouter) Invalidate(host, token string) {
	key := routeKey(host, token)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.primary, key)
	delete(r.notParticipant, key)
}

// InvalidateLogin drops every cached decision involving a login, so a newly
// added or removed account is picked up without a restart.
func (r *loginRouter) InvalidateLogin(loginID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, login := range r.primary {
		if string(login.ID) == loginID {
			delete(r.primary, key)
		}
	}
	for key := range r.notParticipant {
		delete(r.notParticipant[key], loginID)
	}
}

// Warm pre-populates the routing cache from a login's conversation list, so the
// first message in each conversation does not pay for a participation probe.
func (r *loginRouter) Warm(ctx context.Context, login *bridgev2.UserLogin) error {
	client, ok := login.Client.(*NCTalkClient)
	if !ok {
		return fmt.Errorf("login %s has no Nextcloud client", login.ID)
	}
	convs, err := client.Client.ListConversations(ctx)
	if err != nil {
		return err
	}

	host := client.host()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range convs {
		if !convs[i].Bridgeable() {
			continue
		}
		key := routeKey(host, convs[i].Token)
		// Do not displace an already-chosen owner: portals should keep the same
		// owning login across restarts and across other users logging in.
		if existing, ok := r.primary[key]; ok && existing != nil {
			if existing.ID <= login.ID {
				continue
			}
		}
		r.primary[key] = login
	}
	return nil
}
