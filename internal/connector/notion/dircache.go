package notion

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// DirCache memoizes the two lookups the renderer pipeline does
// repeatedly per page: database title (parent.database_id →
// "Project Tracker") and user identity (last_edited_by.id
// → display name + email).
//
// Both lookups are read-mostly, low-cardinality (tens of databases,
// hundreds of users in the typical workspace), and tolerate stale
// values — a renamed database showing the old title for one
// reconcile cycle is fine. The cache is per-process and not
// persisted; pod restarts re-warm lazily.
//
// Goroutine-safe. Stamping the same key concurrently is safe but
// performs the lookup twice — single-flighting is a v2 concern if
// it shows up in metrics.

// dbTitleCacheSize bounds the database-title cache. 256 is
// generous for a workspace with tens of databases.
const dbTitleCacheSize = 256

// userCacheSize bounds the user lookup cache. 1024 covers a
// workspace's members plus external collaborators with margin.
const userCacheSize = 1024

// DirCache is the directory cache. Construct via NewDirCache;
// fields are not safe to set directly.
type DirCache struct {
	client *Client

	mu       sync.Mutex
	dbTitles *lru[string, string] // empty value caches "no access"
	users    *lru[string, *User]  // nil value caches "lookup failed"
}

// NewDirCache constructs a DirCache backed by client. client is
// required — every miss issues an API call.
func NewDirCache(client *Client) *DirCache {
	return &DirCache{
		client:   client,
		dbTitles: newLRU[string, string](dbTitleCacheSize),
		users:    newLRU[string, *User](userCacheSize),
	}
}

// DatabaseTitle resolves a database UUID to its plain-text title.
// On 403/404 (integration doesn't have access to the parent
// database — common when only a row is shared), caches the empty
// string and proceeds. Don't fail the row's ingest because we
// can't resolve its parent database title — the row's own
// metadata is still valuable.
//
// Other errors propagate; the caller decides whether to skip the
// row or retry.
func (c *DirCache) DatabaseTitle(ctx context.Context, id string) (string, error) {
	c.mu.Lock()
	if v, ok := c.dbTitles.get(id); ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	db, err := c.client.GetDatabase(ctx, id)
	if err != nil {
		if IsNotFound(err) || IsForbidden(err) {
			c.mu.Lock()
			c.dbTitles.put(id, "")
			c.mu.Unlock()
			return "", nil
		}
		return "", err
	}
	title := plainText(db.Title)
	c.mu.Lock()
	c.dbTitles.put(id, title)
	c.mu.Unlock()
	return title, nil
}

// User resolves a user UUID to its full User object (email +
// name + bot/person discriminator). Returns (nil, nil) on
// 403/404 — Notion sometimes withholds user details depending on
// integration scope; the caller falls back to the last_edited_by
// PartialUser's name (which the wire shape doesn't always carry).
func (c *DirCache) User(ctx context.Context, id string) (*User, error) {
	if id == "" {
		return nil, errors.New("dircache: empty user id")
	}
	c.mu.Lock()
	if v, ok := c.users.get(id); ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	u, err := c.client.GetUser(ctx, id)
	if err != nil {
		if IsNotFound(err) || IsForbidden(err) {
			c.mu.Lock()
			c.users.put(id, nil)
			c.mu.Unlock()
			return nil, nil
		}
		return nil, err
	}
	c.mu.Lock()
	c.users.put(id, u)
	c.mu.Unlock()
	return u, nil
}

// PrewarmDatabase manually inserts a known database into the
// title cache. Used by the reconcile walker when it encounters a
// child_database block — the walker already has the parent
// Database object in hand from a previous fetch and can warm the
// cache without an extra API call.
func (c *DirCache) PrewarmDatabase(id, title string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dbTitles.put(id, title)
}

// PrewarmUser inserts a known User into the user cache. Same
// motivation as PrewarmDatabase.
func (c *DirCache) PrewarmUser(u *User) {
	if u == nil || u.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users.put(u.ID, u)
}

// lru is a generic, non-goroutine-safe LRU keyed on K with values
// V. Caller wraps it in a mutex (see DirCache). Implementation:
// container/list for recency ordering, map for O(1) lookup.
type lru[K comparable, V any] struct {
	cap   int
	ll    *list.List
	index map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	key K
	val V
}

func newLRU[K comparable, V any](cap int) *lru[K, V] {
	if cap <= 0 {
		cap = 1
	}
	return &lru[K, V]{
		cap:   cap,
		ll:    list.New(),
		index: make(map[K]*list.Element, cap),
	}
}

func (l *lru[K, V]) get(k K) (V, bool) {
	e, ok := l.index[k]
	if !ok {
		var zero V
		return zero, false
	}
	l.ll.MoveToFront(e)
	return e.Value.(*lruEntry[K, V]).val, true
}

func (l *lru[K, V]) put(k K, v V) {
	if e, ok := l.index[k]; ok {
		e.Value.(*lruEntry[K, V]).val = v
		l.ll.MoveToFront(e)
		return
	}
	e := l.ll.PushFront(&lruEntry[K, V]{key: k, val: v})
	l.index[k] = e
	if l.ll.Len() > l.cap {
		oldest := l.ll.Back()
		if oldest != nil {
			l.ll.Remove(oldest)
			delete(l.index, oldest.Value.(*lruEntry[K, V]).key)
		}
	}
}
