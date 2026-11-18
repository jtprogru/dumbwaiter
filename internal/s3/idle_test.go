package s3

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type idleTestBody struct {
	reader io.Reader
	closed chan struct{}
	once   sync.Once
	closes atomic.Int64
}

func (b *idleTestBody) Read(p []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	if b.reader != nil {
		return b.reader.Read(p)
	}
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *idleTestBody) Close() error {
	b.closes.Add(1)
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestIdleReaderDoesNotTimeDownstreamWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := &idleTestBody{reader: strings.NewReader("abc"), closed: make(chan struct{})}
		r := newIdleReader(body, time.Second)
		defer r.Close()
		p := make([]byte, 1)
		for _, want := range []byte("abc") {
			// Also covers the interval before the first Read.
			time.Sleep(2 * time.Second)
			if n, err := r.Read(p); err != nil || n != 1 || p[0] != want {
				t.Fatalf("body closed during downstream work: n=%d data=%q err=%v", n, p, err)
			}
		}
	})
}

func TestIdleReaderClosesStalledReadOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := &idleTestBody{closed: make(chan struct{})}
		r := newIdleReader(body, time.Second)
		start := time.Now()
		_, err := r.Read(make([]byte, 1))
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Second {
			t.Fatalf("stalled read: err=%v elapsed=%v", err, time.Since(start))
		}
		r.Close()
		if body.closes.Load() != 1 {
			t.Fatal("underlying body closed more than once")
		}
	})
}

func TestIdleReaderCloseUnblocksRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := &idleTestBody{closed: make(chan struct{})}
		r := newIdleReader(body, time.Second)
		done := make(chan error, 1)
		go func() { _, err := r.Read(make([]byte, 1)); done <- err }()
		synctest.Wait()
		r.Close()
		if err := <-done; !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("read after close: %v", err)
		}
		time.Sleep(2 * time.Second)
		if body.closes.Load() != 1 {
			t.Fatal("callback survived completed Read")
		}
	})
}
