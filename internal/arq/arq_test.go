// internal/arq/arq_test.go
package arq

import (
	"sync"
	"testing"
	"time"

	"github.com/sparques/hdlc/internal/wire"
)

type fakeSender struct {
	mu      sync.Mutex
	frames  []wire.Frame
	failAll bool
}

func (s *fakeSender) SendFrame(id uint16, t wire.FrameType, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, wire.Frame{ID: id, Type: t, Payload: append([]byte(nil), payload...)})
	return nil
}

func TestStopAndWaitSuccess(t *testing.T) {
	sender := &fakeSender{}
	sw := NewStopAndWait(sender, 20*time.Millisecond, 3)
	defer sw.Close()

	data := []byte("hello world")
	var (
		frameID uint16
		err     error
	)

	// send in background; ack shortly afterwards
	done := make(chan struct{})
	go func() {
		frameID, err = sw.Send(data)
		close(done)
	}()

	// wait until something is on the wire
	time.Sleep(10 * time.Millisecond)

	sender.mu.Lock()
	if len(sender.frames) != 1 {
		sender.mu.Unlock()
		t.Fatalf("expected 1 frame sent, got %d", len(sender.frames))
	}
	f := sender.frames[0]
	sender.mu.Unlock()

	if string(f.Payload) != string(data) {
		t.Fatalf("payload mismatch: got %q", f.Payload)
	}

	// ack it
	sw.HandleAck(f.ID, false)

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Send did not return after ACK")
	}

	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if frameID != f.ID {
		t.Fatalf("frameID mismatch: got %d, want %d", frameID, f.ID)
	}
}

func TestStopAndWaitExhaustRetries(t *testing.T) {
	sender := &fakeSender{}
	sw := NewStopAndWait(sender, 20*time.Millisecond, 2)
	defer sw.Close()

	data := []byte("lost in space")

	done := make(chan error, 1)
	go func() {
		_, err := sw.Send(data)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after retries exhausted, got nil")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Send did not fail within expected time")
	}
}
