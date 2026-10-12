package proxy

import (
	"context"
	"io"
	"time"
)

// Canceling the HTTP request interrupts a stalled transport Read without a
// goroutine per chunk, while permitting arbitrarily long active streams.
type idleReader struct {
	reader io.Reader
	timer  *time.Timer
	idle   time.Duration
}

func newIdleReader(reader io.Reader, idle time.Duration, cancel context.CancelFunc) *idleReader {
	return &idleReader{reader: reader, timer: time.AfterFunc(idle, cancel), idle: idle}
}

func (r *idleReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	return n, err
}

func (r *idleReader) stop() {
	r.timer.Stop()
}
