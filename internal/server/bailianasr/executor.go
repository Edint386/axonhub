package bailianasr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

// resultEventType is the stream event that ends an async job; it carries the same transcript
// envelope as the non-stream response.
const resultEventType = "bailian.asr.result"

// committedEventType is the first stream event once DashScope accepted the work (an async task
// was submitted, or a -message task started). TransformStream turns it into a response the
// pipeline counts as content, so the attempt commits and the SSE response (with keep-alive
// comments) starts while the transcript is still being made; the client never sees it.
const committedEventType = "bailian.asr.committed"

// jobExecutor runs async transcription jobs and -message tasks, guards the synchronous flash call
// (see doFlash), and passes every other request to the inner executor unchanged.
type jobExecutor struct {
	inner pipeline.Executor
	cfg   Config
	cache *jobCache
}

func (e *jobExecutor) Do(ctx context.Context, req *httpclient.Request) (*httpclient.Response, error) {
	var (
		body []byte
		err  error
	)

	if j, ok := jobOf(req, kindAsync); ok {
		body, err = e.runEnvelope(ctx, req, j)
	} else if j, ok := jobOf(req, kindMessage); ok {
		body, err = e.runMessageEnvelope(ctx, req, j)
	} else if isFlash(req) {
		return e.doFlash(ctx, req)
	} else {
		return e.inner.Do(ctx, req)
	}

	if err != nil {
		return nil, err
	}

	return &httpclient.Response{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{contentTypeJSON}},
		Body:       body,
		Request:    req,
	}, nil
}

// DoStream uploads and submits an async job (or starts a -message task) before returning, so
// those errors stay retryable. The returned stream starts with the committed event and then
// waits for the result while the handler sends keep-alive comments.
func (e *jobExecutor) DoStream(ctx context.Context, req *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
	if j, ok := jobOf(req, kindMessage); ok {
		sess, err := e.startMessage(ctx, req, j)
		if err != nil {
			return nil, err
		}

		return newProducerStream(ctx, nil, func(ctx context.Context, emit func(*httpclient.StreamEvent) error) error {
			if err := emit(committedEvent(sess.taskID)); err != nil {
				sess.close()
				return err
			}

			return sess.run(ctx, emit)
		}), nil
	}

	j, ok := jobOf(req, kindAsync)
	if !ok {
		if isFlash(req) {
			return e.doFlashStream(ctx, req)
		}

		return e.inner.DoStream(ctx, req)
	}

	key, cached, task, err := e.start(ctx, req, j)
	if err != nil {
		return nil, err
	}

	if cached != nil {
		body, err := e.envelope(cached, j)
		if err != nil {
			return nil, err
		}

		return streams.SliceStream([]*httpclient.StreamEvent{{Type: resultEventType, Data: body}}), nil
	}

	return newProducerStream(ctx, []*httpclient.StreamEvent{committedEvent(task.id)},
		func(ctx context.Context, emit func(*httpclient.StreamEvent) error) error {
			t, err := e.finish(ctx, j, key, task)
			if err != nil {
				return err
			}

			body, err := e.envelope(t, j)
			if err != nil {
				return err
			}

			return emit(&httpclient.StreamEvent{Type: resultEventType, Data: body})
		}), nil
}

func committedEvent(taskID string) *httpclient.StreamEvent {
	data, _ := json.Marshal(map[string]string{"task_id": taskID})
	return &httpclient.StreamEvent{Type: committedEventType, Data: data}
}

func jobOf(req *httpclient.Request, kind string) (*job, bool) {
	if req == nil || req.Metadata[metaKind] != kind {
		return nil, false
	}

	j, ok := req.TransformerMetadata[jobMetadataKey].(*job)

	return j, ok && j != nil
}

// runMessageEnvelope runs one whole -message task and returns its transcript envelope.
func (e *jobExecutor) runMessageEnvelope(ctx context.Context, req *httpclient.Request, j *job) ([]byte, error) {
	sess, err := e.startMessage(ctx, req, j)
	if err != nil {
		return nil, err
	}

	folder := &messageFolder{}

	err = sess.run(ctx, func(ev *httpclient.StreamEvent) error {
		_, err := folder.fold(ev)
		return err
	})
	if err != nil {
		return nil, err
	}

	return e.envelope(folder.transcript(), j)
}

func (e *jobExecutor) runEnvelope(ctx context.Context, req *httpclient.Request, j *job) ([]byte, error) {
	t, err := e.run(ctx, req, j)
	if err != nil {
		return nil, err
	}

	return e.envelope(t, j)
}

func (e *jobExecutor) envelope(t *transcript, j *job) ([]byte, error) {
	if !j.opts.Words {
		t = t.withoutWords()
	}

	body, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("failed to encode bailian transcription result: %w", err)
	}

	return body, nil
}

// taskRef is a submitted task and the DashScope headers it was submitted with; a task can only
// be queried by the account that created it. resumable tells whether the job cache keeps it, so
// that a repeated request resumes it.
type taskRef struct {
	id        string
	headers   http.Header
	resumable bool
	// submitted is when DashScope accepted the task (see dashScopeTaskRetention); zero when
	// unknown.
	submitted time.Time
	// fresh: this request submitted the task just now, or joined the submit that did, rather
	// than being handed an older task by the job cache. DashScope accepted it with a task_id, so
	// it runs and is billed whatever its status reads later (see unknownTaskError).
	fresh bool
	// foreign: the task is queried with credentials other than this request's own (another
	// request submitted it with another key), so a rejected query says nothing about the key or
	// channel this request was given (see pollError).
	foreign bool
	// own: this request uploaded and submitted the task itself, rather than joining another
	// request's submit or resuming a task the job cache held (see ReusedJob).
	own bool
}

// maxSharedSubmitRestarts bounds how often a request starts its upload again, with its own
// credentials, after the shared upload it joined failed in another request (see submitOnce).
const maxSharedSubmitRestarts = 3

// run returns the job result, submitting the audio at most once per cache key: a retry polls
// the task the first attempt started, or reuses its result.
func (e *jobExecutor) run(ctx context.Context, req *httpclient.Request, j *job) (*transcript, error) {
	key, cached, task, err := e.start(ctx, req, j)
	if err != nil || cached != nil {
		return cached, err
	}

	return e.finish(ctx, j, key, task)
}

// start returns a cached result, or the task to wait for (submitting the audio unless the cache
// already knows a running task for it). Audio given by URL bypasses the cache: the gateway
// cannot tell whether the content behind the URL changed, so every request submits it (and
// the returned key is empty).
func (e *jobExecutor) start(ctx context.Context, req *httpclient.Request, j *job) (string, *transcript, taskRef, error) {
	if _, remote := j.audio.(RemoteAudio); remote {
		task, err := e.submitNew(ctx, req, j)
		return "", nil, task, err
	}

	digest, err := j.audio.Digest()
	if err != nil {
		return "", nil, taskRef{}, fmt.Errorf("failed to hash audio: %w", err)
	}

	key := jobCacheKey(e.cfg.CacheScope, callerScope(ctx), req, digest)
	if entry, ok := e.cache.lookup(key); ok && entry.result != nil {
		j.usage.Store(entry.usage)
		j.reused.Store(true)
		notifyStaged(j.audio)

		return key, entry.result, taskRef{id: entry.taskID, headers: entry.headers, resumable: true, submitted: entry.submitted}, nil
	}

	task, err := e.submitOnce(ctx, key, req, j)
	if err != nil {
		return "", nil, taskRef{}, err
	}

	task.foreign = task.headers.Get("Authorization") != dashScopeHeaders(req).Get("Authorization")
	if !task.own {
		j.reused.Store(true)
	}

	// Another request's upload (or an earlier task) serves this one: its own audio is not read.
	notifyStaged(j.audio)

	return key, nil, task, nil
}

// finish waits for a submitted task and caches its result. A task that may still finish, or that
// finished but whose result this request could not read, stays cached from the end of this
// request on, so a repeated request resumes it instead of paying for it again; only a task that
// failed unbilled is forgotten. A task outside the cache (an empty key) is only waited for.
func (e *jobExecutor) finish(ctx context.Context, j *job, key string, task taskRef) (*transcript, error) {
	if key == "" {
		return e.await(ctx, j, task, nil)
	}

	t, err := e.await(ctx, j, task, func() { e.cache.touch(key, task) })
	if err != nil {
		if isJobFailure(err) {
			e.cache.forgetTask(key, task.id)
		} else {
			e.cache.touch(key, task)
		}

		return nil, err
	}

	j.usage.Store(e.cache.putResult(key, task.id, t))

	return t, nil
}

// submitOnce returns the running task for key, or uploads and submits the audio once for all
// concurrent identical requests. The shared upload runs with the context and the credentials of
// the request that started it (the leader). A request that joined it and gets back a failure that
// may be the leader's own (see failedForTheLeaderOnly: DashScope refused or throttled the leader's
// key, or the leader's client went away) starts over with its own credentials instead of taking
// that failure as its own. Every other failure is shared: one that would fail the same way for
// every request (a stalled endpoint, rejected audio), and a billed failure or a lost submit answer
// (pipeline.ErrNoRetry), since another submit would pay again. A shared failure, like one the
// joined request still gets after maxSharedSubmitRestarts, is reported as not its own (see
// joinedSubmitError and IsChannelNeutralFailure).
func (e *jobExecutor) submitOnce(ctx context.Context, key string, req *httpclient.Request, j *job) (taskRef, error) {
	for restarts := 0; ; restarts++ {
		led := false

		value, err, _ := e.cache.submits.Do(key, func() (any, error) {
			led = true

			task, err := e.leadSubmit(ctx, key, req, j)
			if err != nil {
				// The joined requests learn whether the leader's own context had ended.
				return sharedSubmitFailure{leaderGone: ctx.Err() != nil}, err
			}

			return task, nil
		})
		if err == nil {
			task := value.(taskRef)
			task.own = task.own && led

			return task, nil
		}

		if led {
			return taskRef{}, err
		}

		failure, _ := value.(sharedSubmitFailure)
		if restarts < maxSharedSubmitRestarts && ctx.Err() == nil && failedForTheLeaderOnly(err, failure.leaderGone) {
			continue
		}

		return taskRef{}, &joinedSubmitError{err: err}
	}
}

// leadSubmit is the shared submit of submitOnce, run by the request that leads it.
func (e *jobExecutor) leadSubmit(ctx context.Context, key string, req *httpclient.Request, j *job) (taskRef, error) {
	if entry, ok := e.cache.lookup(key); ok && entry.pending {
		return taskRef{}, pendingSubmitError()
	}

	if task, ok := e.resumable(ctx, key, j); ok {
		return task, nil
	}

	task, err := e.submitNew(ctx, req, j)
	if err != nil {
		if isUnconfirmedSubmit(err) {
			e.cache.putPending(key)
		}

		return taskRef{}, err
	}

	task.resumable = true
	e.cache.putTask(key, task.id, task.headers)

	return task, nil
}

// sharedSubmitFailure is what a failed shared submit hands to the requests that joined it,
// besides the error.
type sharedSubmitFailure struct {
	// leaderGone: the leading request's context had ended (its client left, or its deadline
	// passed) when the submit failed.
	leaderGone bool
}

// failedForTheLeaderOnly reports a failure of the shared submit that may depend on the leading
// request itself, so a request that joined it may succeed on its own: DashScope refused (401,
// 403) or throttled (429) the leader's credentials, or the leader's context ended. A failure that
// carries pipeline.ErrNoRetry (billed, or a submit whose answer was lost) never qualifies, nor
// does one that would fail the same way for every request: a step that got no answer within the
// start timeout (startTimeoutError), audio DashScope rejected (400, 413), a transport failure.
func failedForTheLeaderOnly(err error, leaderGone bool) bool {
	if errors.Is(err, pipeline.ErrNoRetry) {
		return false
	}

	if leaderGone {
		return true
	}

	var status int

	// DashScope reports some credential failures (Arrearage, AccessDenied*) as HTTP 400, so the
	// error code decides, mapped the way statusForCode maps it for the client.
	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		status = httpErr.StatusCode
		if code, _, _, ok := dashScopeError(httpErr.Body); ok {
			status = statusForCode(code, status)
		}
	} else if respErr, ok := errors.AsType[*llm.ResponseError](err); ok {
		status = statusForCode(respErr.Detail.Code, respErr.StatusCode)
	}

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// resumable returns the running task the cache knows for key. That task was submitted by an
// earlier request, so it is queried once first with the headers it was submitted with: when the
// query is rejected for good (the submitting key was revoked, or the task is gone), the entry
// is dropped and the caller submits the audio again with its own credentials.
func (e *jobExecutor) resumable(ctx context.Context, key string, j *job) (taskRef, bool) {
	entry, ok := e.cache.lookup(key)
	if !ok {
		return taskRef{}, false
	}

	task := taskRef{id: entry.taskID, headers: entry.headers, resumable: true, submitted: entry.submitted}

	probeCtx, cancel := context.WithTimeout(ctx, e.cfg.startTimeout())
	_, err := e.queryTask(probeCtx, j, task)

	cancel()

	if _, rejected := errors.AsType[*httpclient.Error](err); rejected && !isTransient(err) && ctx.Err() == nil {
		log.Warn(ctx, "bailian transcription task can no longer be queried with its submitting credentials; submitting again",
			log.String("task_id", task.id), log.Cause(err))
		e.cache.forgetTask(key, task.id)

		return taskRef{}, false
	}

	return task, true
}

// submitNew uploads (unless the audio is remote) and submits the audio as a new task.
func (e *jobExecutor) submitNew(ctx context.Context, req *httpclient.Request, j *job) (taskRef, error) {
	fileURL, err := e.stage(ctx, req, j)
	if err != nil {
		return taskRef{}, err
	}

	// The audio is in Bailian's temporary storage: the request no longer needs to hold what it
	// reserved for the upload (a retry that uploads again reads the source all the same).
	notifyStaged(j.audio)

	taskID, err := e.submit(ctx, req, fileURL)
	if err != nil {
		return taskRef{}, err
	}

	return taskRef{id: taskID, headers: dashScopeHeaders(req), fresh: true, own: true, submitted: time.Now()}, nil
}

// stage makes the audio reachable by DashScope: remote audio as is, anything else through the
// temporary storage upload.
func (e *jobExecutor) stage(ctx context.Context, req *httpclient.Request, j *job) (string, error) {
	if remote, ok := j.audio.(RemoteAudio); ok {
		return remote.URL(), nil
	}

	return e.upload(ctx, req, j)
}
