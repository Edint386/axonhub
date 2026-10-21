package realtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func dialPair(t *testing.T) (server *websocket.Conn, client *websocket.Conn) {
	t.Helper()

	var (
		mu    sync.Mutex
		sconn *websocket.Conn
		ready = make(chan struct{})
	)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		mu.Lock()
		sconn = c
		mu.Unlock()
		close(ready)
	}))
	t.Cleanup(srv.Close)

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)

	<-ready

	mu.Lock()
	defer mu.Unlock()

	return sconn, c
}

func TestSocketWriter_OrderAndClose(t *testing.T) {
	server, client := dialPair(t)
	cfg := testConfig()

	w := newSocketWriter("srv", server, cfg)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go w.run(ctx)

	for i := range 5 {
		require.NoError(t, w.enqueue(ctx, websocket.TextMessage, []byte{byte('0' + i)}))
	}

	w.enqueueClose(websocket.ClosePolicyViolation, "stop")
	require.ErrorIs(t, w.enqueue(ctx, websocket.TextMessage, []byte("late")), errWriterClosed)

	for i := range 5 {
		_, data, err := client.ReadMessage()
		require.NoError(t, err)
		require.Equal(t, string(rune('0'+i)), string(data))
	}

	_, _, err := client.ReadMessage()

	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, websocket.ClosePolicyViolation, ce.Code)
	require.Equal(t, "stop", ce.Text)

	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not finish after close")
	}
}

func TestSocketWriter_BackpressureBlocksThenDrains(t *testing.T) {
	server, client := dialPair(t)
	cfg := testConfig()
	cfg.WriteQueueFrames = 2

	w := newSocketWriter("srv", server, cfg)

	// Not running yet: enqueue must block once the queue is full.
	ctx := t.Context()
	require.NoError(t, w.enqueue(ctx, websocket.TextMessage, []byte("1")))
	require.NoError(t, w.enqueue(ctx, websocket.TextMessage, []byte("2")))

	blocked := make(chan error, 1)

	go func() {
		blocked <- w.enqueue(ctx, websocket.TextMessage, []byte("3"))
	}()

	select {
	case <-blocked:
		t.Fatal("enqueue should block while the queue is full")
	case <-time.After(100 * time.Millisecond):
	}

	go w.run(ctx)

	require.NoError(t, <-blocked)

	for _, want := range []string{"1", "2", "3"} {
		_, data, err := client.ReadMessage()
		require.NoError(t, err)
		require.Equal(t, want, string(data))
	}
}

func TestSocketWriter_EnqueueHonoursContext(t *testing.T) {
	server, _ := dialPair(t)
	cfg := testConfig()
	cfg.WriteQueueFrames = 1

	w := newSocketWriter("srv", server, cfg)
	require.NoError(t, w.enqueue(t.Context(), websocket.TextMessage, []byte("1")))

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := w.enqueue(ctx, websocket.TextMessage, []byte("2"))
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestSocketWriter_OversizeFrameFitsWhenQueueEmpty(t *testing.T) {
	server, client := dialPair(t)
	cfg := testConfig()
	cfg.WriteQueueBytes = 10

	w := newSocketWriter("srv", server, cfg)
	ctx := t.Context()

	go w.run(ctx)

	big := strings.Repeat("x", 1000)
	require.NoError(t, w.enqueue(ctx, websocket.TextMessage, []byte(big)))

	_, data, err := client.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, big, string(data))
}
