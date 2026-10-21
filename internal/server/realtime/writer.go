package realtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/looplj/axonhub/internal/log"
)

var (
	errWriterClosed = errors.New("realtime: socket writer closed")
	errWriterFailed = errors.New("realtime: socket writer failed")
)

// writeItem is one queued frame or a close request.
type writeItem struct {
	msgType int
	data    []byte
	close   bool
	code    int
	reason  string
}

// socketWriter is the single writer goroutine of one WebSocket connection. Every producer
// (both pumps, the dialect's synthesized events, accounting, timers) enqueues through it, so
// frames reach the socket in strict FIFO order and gorilla's one-writer rule holds.
//
// Enqueue applies backpressure: it blocks while the queue holds cfg.WriteQueueFrames items or
// while adding the frame would exceed cfg.WriteQueueBytes and the queue is not empty (so one
// maximum-size frame always fits). A close is an ordinary item: it is written after everything
// queued before it, and the writer accepts no data afterwards.
type socketWriter struct {
	conn *websocket.Conn
	cfg  Config
	name string

	mu       sync.Mutex
	cond     *sync.Cond
	queue    []writeItem
	bytes    int64
	closing  bool // a close item was queued; no more data accepted
	stopped  bool // the run loop exited
	err      error
	done     chan struct{}
	doneOnce sync.Once
}

func newSocketWriter(name string, conn *websocket.Conn, cfg Config) *socketWriter {
	w := &socketWriter{conn: conn, cfg: cfg, name: name, done: make(chan struct{})}
	w.cond = sync.NewCond(&w.mu)

	return w
}

// run drains the queue until a close item was written, the connection fails, or stop is
// called. It must run in its own goroutine.
func (w *socketWriter) run(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error(ctx, "panic in realtime socket writer", log.String("socket", w.name), log.Any("panic", recovered))
		}

		w.finish(errWriterClosed)
	}()

	for {
		item, ok := w.next()
		if !ok {
			return
		}

		if item.close {
			deadline := time.Now().Add(w.cfg.WriteTimeout)
			if err := w.conn.WriteControl(websocket.CloseMessage, closePayload(item.code, item.reason), deadline); err != nil &&
				!errors.Is(err, websocket.ErrCloseSent) {
				log.Debug(ctx, "realtime close frame write failed", log.String("socket", w.name), log.Cause(err))
			}

			return
		}

		if err := w.conn.SetWriteDeadline(time.Now().Add(w.cfg.WriteTimeout)); err != nil {
			w.finish(err)
			return
		}

		if err := w.conn.WriteMessage(item.msgType, item.data); err != nil {
			w.finish(err)
			return
		}
	}
}

// next blocks until an item is available or the writer is stopped.
func (w *socketWriter) next() (writeItem, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for len(w.queue) == 0 && !w.stopped {
		w.cond.Wait()
	}

	if w.stopped && len(w.queue) == 0 {
		return writeItem{}, false
	}

	item := w.queue[0]
	w.queue[0] = writeItem{}
	w.queue = w.queue[1:]
	w.bytes -= int64(len(item.data))
	w.cond.Broadcast()

	return item, true
}

// enqueue queues a data frame, blocking for queue space. It returns an error once the writer
// is closing, failed, or ctx is done.
func (w *socketWriter) enqueue(ctx context.Context, msgType int, data []byte) error {
	stop := context.AfterFunc(ctx, func() {
		w.mu.Lock()
		w.cond.Broadcast()
		w.mu.Unlock()
	})
	defer stop()

	w.mu.Lock()
	defer w.mu.Unlock()

	for {
		if w.stopped {
			if w.err != nil {
				return w.err
			}

			return errWriterClosed
		}

		if w.closing {
			return errWriterClosed
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if len(w.queue) < w.cfg.WriteQueueFrames && (len(w.queue) == 0 || w.bytes+int64(len(data)) <= w.cfg.WriteQueueBytes) {
			break
		}

		w.cond.Wait()
	}

	w.queue = append(w.queue, writeItem{msgType: msgType, data: data})
	w.bytes += int64(len(data))
	w.cond.Broadcast()

	return nil
}

// enqueueClose queues a close frame after all pending data and stops accepting data. It never
// blocks and is idempotent.
func (w *socketWriter) enqueueClose(code int, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closing || w.stopped {
		return
	}

	w.closing = true
	w.queue = append(w.queue, writeItem{close: true, code: code, reason: reason})
	w.cond.Broadcast()
}

// stop ends the run loop without writing anything further (used after the connection was
// force-closed).
func (w *socketWriter) stop() {
	w.finish(errWriterClosed)
}

func (w *socketWriter) finish(err error) {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		if err != nil && w.err == nil && !errors.Is(err, errWriterClosed) {
			w.err = err
		}
	}

	w.queue = nil
	w.bytes = 0
	w.cond.Broadcast()
	w.mu.Unlock()

	w.doneOnce.Do(func() { close(w.done) })
}

// failed reports whether a write error ended the writer.
func (w *socketWriter) failed() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.err != nil {
		return errors.Join(errWriterFailed, w.err)
	}

	return nil
}

// ping sends a ping control frame; safe to call concurrently with the writer.
func (w *socketWriter) ping() error {
	return w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(w.cfg.WriteTimeout))
}

// emergencyClose writes a close frame directly, bypassing the queue, for the case where the
// writer is stuck on a slow peer. It is only used right before the connection is force-closed.
func (w *socketWriter) emergencyClose(code int, reason string) {
	_ = w.conn.WriteControl(websocket.CloseMessage, closePayload(code, reason), time.Now().Add(time.Second))
}
