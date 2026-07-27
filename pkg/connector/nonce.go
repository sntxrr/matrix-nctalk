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
	"sync"
	"time"
)

// Defaults for the replay cache. The retention is what bounds how long a
// captured request stays useful; the size is what bounds memory when something
// floods the endpoint.
const (
	defaultNonceRetention = 15 * time.Minute
	defaultNonceMaxSize   = 16384
)

// nonceCache remembers the randoms of recently accepted webhooks so a captured
// request cannot be replayed.
//
// Talk signs each webhook over its X-Nextcloud-Talk-Random plus the body and
// never reuses a random, so a repeat is either a replay or a retransmission,
// and neither should be processed twice.
//
// Entries are dropped by rotating two generations rather than by timing each
// one out: a lookup checks both halves, and a rotation discards the older. That
// keeps insertion allocation-free per entry and bounds memory without a sweep
// goroutine. An entry therefore lives between one and two retention periods,
// which is the right direction to be imprecise in.
type nonceCache struct {
	retention time.Duration
	maxSize   int
	// now is overridable so tests do not have to sleep.
	now func() time.Time

	mu         sync.Mutex
	current    map[string]struct{}
	previous   map[string]struct{}
	rotatedAt  time.Time
	rotations  int
	rejections int
}

func newNonceCache(retention time.Duration, maxSize int) *nonceCache {
	if retention <= 0 {
		retention = defaultNonceRetention
	}
	if maxSize <= 0 {
		maxSize = defaultNonceMaxSize
	}
	return &nonceCache{
		retention: retention,
		maxSize:   maxSize,
		now:       time.Now,
		current:   make(map[string]struct{}),
		previous:  make(map[string]struct{}),
	}
}

// Accept records a random and reports whether it is new. A false return means
// the request is a replay and must not be processed.
func (n *nonceCache) Accept(random string) bool {
	if random == "" {
		return false
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	now := n.now()
	if n.rotatedAt.IsZero() {
		n.rotatedAt = now
	}
	// Rotating on size as well as age is what keeps a flood from growing the
	// cache without bound; it costs only a shorter memory of what was seen.
	if now.Sub(n.rotatedAt) >= n.retention || len(n.current) >= n.maxSize {
		n.previous = n.current
		n.current = make(map[string]struct{})
		n.rotatedAt = now
		n.rotations++
	}

	if _, seen := n.current[random]; seen {
		n.rejections++
		return false
	}
	if _, seen := n.previous[random]; seen {
		n.rejections++
		return false
	}

	n.current[random] = struct{}{}
	return true
}

// stats reports what the cache has done, for tests and diagnostics.
func (n *nonceCache) stats() (size, rotations, rejections int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.current) + len(n.previous), n.rotations, n.rejections
}
