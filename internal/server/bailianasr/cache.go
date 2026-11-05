package bailianasr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	jobCacheTTL        = 30 * time.Minute
	jobCacheMaxEntries = 512

	// dashScopeTaskRetention is how long DashScope keeps a task queryable after its submit. A
	// task the job cache holds was accepted with a task_id, so within this time a status that
	// does not find it (UNKNOWN) never means the task is gone (see unknownTaskError).
	dashScopeTaskRetention = 24 * time.Hour

	// pendingSubmitTTL is how long a submit whose answer was lost blocks an identical request
	// (see jobCache.putPending).
	pendingSubmitTTL = 10 * time.Minute
)

// sharedJobCache is process-wide so a retry reaches the task its first attempt submitted even
// when the channel was rebuilt in between, or when the load balancer sends it to another Bailian
// channel. It lives in memory only: a gateway restart forgets every task.
var sharedJobCache = newJobCache(jobCacheTTL, jobCacheMaxEntries)

// jobCache remembers submitted DashScope tasks. A retried attempt (pipeline retry or a client
// repeating the request) from the same caller with the same DashScope endpoint, model,
// parameters and audio polls the task already running, or returns its result, instead of
// uploading and paying for the audio again; that includes a billed task whose result a request
// could not read. Concurrent identical requests share one upload and submit. Audio given by URL
// never enters the cache (see urlSource.Digest).
//
// The key leaves the channel and its API key out, so a resend that the load balancer gives to
// another Bailian channel, or that a channel serves with another of its keys, still finds the
// task; the entry keeps the submitting request's DashScope headers, because a task can only be
// queried by the account that created it (a resend whose task can no longer be queried with
// them submits again; see jobExecutor.resumable). The key does hold the caller (see
// callerScope), so one caller never receives, or learns about, another caller's job. An entry
// lives jobCacheTTL from its last use: every poll and the end of every request that leaves the
// task running refresh it, so "send the same request again within 30 minutes" holds from the
// moment the request ended. A submit whose answer was lost leaves a pending entry instead
// (pendingSubmitTTL), so an identical request does not pay for the audio a second time while the
// first task may still run.
type jobCache struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	entries map[string]*cacheEntry
	submits singleflight.Group
}

type cacheEntry struct {
	taskID string
	// submitted is when DashScope accepted the task (see dashScopeTaskRetention).
	submitted time.Time
	// headers are the DashScope headers (key, workspace, channel overrides) the task was
	// submitted with; polls use them.
	headers http.Header
	result  *transcript
	// usage goes with result: the one request that records the usage of the task takes it.
	usage *usageClaim
	// pending marks a submit whose answer was lost (no task ID): DashScope may run, and bill, a
	// task nobody can query. An identical request is refused until it expires.
	pending bool
	expires time.Time
}

// usageClaim hands the usage of one finished task to a single request. DashScope bills a task
// once, but the job cache gives its result to every request that asks for the same job (a client
// resending a request whose response it lost, concurrent identical requests): only the first of
// them to deliver the result records the usage (see job.billsUsage).
type usageClaim struct {
	taken atomic.Bool
}

func newJobCache(ttl time.Duration, maxEntries int) *jobCache {
	return &jobCache{ttl: ttl, maxEntries: maxEntries, now: time.Now, entries: map[string]*cacheEntry{}}
}

// jobCacheKey hashes everything that identifies one job: the cache scope (Config.CacheScope,
// empty in production), the caller, the native URL (DashScope host and region, or the relay in
// front of it), the DashScope headers that select the account's workspace or options, the audio
// and the task body (model and parameters). The credentials are not part of it; see jobCache.
func jobCacheKey(scope, caller string, req *httpclient.Request, audioDigest string) string {
	h := sha256.New()
	for _, part := range []string{scope, caller, req.URL, audioDigest} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}

	for _, name := range identityHeaderNames(req.Headers) {
		for _, value := range req.Headers.Values(name) {
			h.Write([]byte(name + ": " + value))
			h.Write([]byte{0})
		}
	}

	h.Write([]byte{0})
	h.Write(req.Body)

	return hex.EncodeToString(h.Sum(nil))
}

// callerScope names whose job a cache entry is: the caller's API key, else its project. A job is
// only shared within it, so another caller can neither receive the result nor tell from a fast
// answer that the same audio was transcribed recently, and a header that renders per caller (a
// workspace or key the caller brings) cannot make one caller's request poll another's task.
func callerScope(ctx context.Context) string {
	if key, ok := contexts.GetAPIKey(ctx); ok {
		return "api_key:" + strconv.Itoa(key.ID)
	}

	if project, ok := contexts.GetProjectID(ctx); ok {
		return "project:" + strconv.Itoa(project)
	}

	return ""
}

// identityHeaderNames are the sorted X-DashScope-* headers of req (an inbound or channel-override
// X-DashScope-WorkSpace, for example), leaving out those that only describe this one call.
// Other headers stay out of the key: clients send per-attempt headers (retry counts, trace IDs)
// that must not turn a resend into a new job.
func identityHeaderNames(headers http.Header) []string {
	var names []string

	for name := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if !strings.HasPrefix(canonical, "X-Dashscope-") ||
			slices.ContainsFunc(requestOnlyHeaders, func(h string) bool { return http.CanonicalHeaderKey(h) == canonical }) {
			continue
		}

		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

func (c *jobCache) lookup(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return cacheEntry{}, false
	}

	if c.now().After(entry.expires) {
		delete(c.entries, key)
		return cacheEntry{}, false
	}

	return *entry, true
}

// putTask stores a task DashScope accepted just now.
func (c *jobCache) putTask(key, taskID string, headers http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.storeLocked(key, &cacheEntry{taskID: taskID, submitted: c.now(), headers: headers})
}

// putPending remembers, for pendingSubmitTTL, that a submit for key was sent but its answer
// (with the task ID) was lost. Until then an identical request gets a 409 instead of submitting,
// and paying for, the same audio again (see pendingSubmitError).
func (c *jobCache) putPending(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.storeLockedFor(key, &cacheEntry{pending: true}, pendingSubmitTTL)
}

// putResult stores the result of taskID and returns the usage claim that goes with it. When a
// request that waited for the same task stored it first (concurrent identical requests share
// one submit, then each polls), its result and claim are kept, so only one of them records the
// usage of the task.
func (c *jobCache) putResult(key, taskID string, result *transcript) *usageClaim {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry := &cacheEntry{taskID: taskID, result: result, usage: &usageClaim{}}
	if old, ok := c.entries[key]; ok && old.taskID == taskID {
		entry.headers, entry.submitted = old.headers, old.submitted
		if old.result != nil {
			entry.result, entry.usage = old.result, old.usage
		}
	}

	c.storeLocked(key, entry)

	return entry.usage
}

// touch restarts the TTL of the entry for a task that is still running (or inserts it again if
// it expired or was evicted), keeping a result a concurrent request may have stored meanwhile.
// A live entry of another task (the audio was submitted again meanwhile, perhaps already with a
// result) is left alone: replacing it would drop that task and make the next resend pay again.
func (c *jobCache) touch(key string, task taskRef) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]

	switch {
	case ok && entry.taskID == task.id:
		entry.expires = c.now().Add(c.ttl)
	case ok && !c.now().After(entry.expires):
		// Another task's entry.
	default:
		c.storeLocked(key, &cacheEntry{taskID: task.id, submitted: task.submitted, headers: task.headers})
	}
}

// forgetTask drops the entry of taskID. An entry another request stored for another task meanwhile
// (the audio was submitted again) is kept: dropping it would make the next resend pay again.
func (c *jobCache) forgetTask(key, taskID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[key]; ok && entry.taskID == taskID {
		delete(c.entries, key)
	}
}

func (c *jobCache) storeLocked(key string, entry *cacheEntry) {
	c.storeLockedFor(key, entry, c.ttl)
}

func (c *jobCache) storeLockedFor(key string, entry *cacheEntry, ttl time.Duration) {
	now := c.now()
	entry.expires = now.Add(ttl)
	c.entries[key] = entry

	if len(c.entries) > c.maxEntries {
		c.evict(now)
	}
}

// evict drops expired entries, then finished results closest to expiry, and only then entries
// of tasks that may still be running (dropping one of those turns a resume into a resubmit).
func (c *jobCache) evict(now time.Time) {
	for key, entry := range c.entries {
		if now.After(entry.expires) {
			delete(c.entries, key)
		}
	}

	for _, results := range []bool{true, false} {
		for len(c.entries) > c.maxEntries {
			oldestKey, found := "", false

			var oldest time.Time

			for key, entry := range c.entries {
				if (entry.result != nil) != results {
					continue
				}

				if !found || entry.expires.Before(oldest) {
					oldestKey, oldest, found = key, entry.expires, true
				}
			}

			if !found {
				break
			}

			delete(c.entries, oldestKey)
		}
	}
}
