// hdlc/hdlc.go
package hdlc

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/sparques/hdlc/internal/arq"
	"github.com/sparques/hdlc/internal/wire"
)

const (
	DefaultMaxPayloadSize = 0xFFFF // uint16 max
	DefaultRetryInterval  = arq.DefaultRetryInterval
	DefaultMaxRetries     = arq.DefaultMaxRetries
)

// Framer is the public interface you requested.
type Framer interface {
	SetAutoFlush(bool)
	io.ReadWriter
}

// Option configures the HDLCFramer.
type Option func(*HDLCFramer)

// WithMaxPayloadSize sets the max payload size per frame (<= 65535).
func WithMaxPayloadSize(n uint16) Option {
	return func(f *HDLCFramer) {
		f.maxPayloadSize = n
	}
}

// WithRetryInterval sets how long we wait before resending an unacked frame.
func WithRetryInterval(d time.Duration) Option {
	return func(f *HDLCFramer) {
		f.retryInterval = d
	}
}

// WithMaxRetries sets the number of retransmissions before giving up.
func WithMaxRetries(n int) Option {
	return func(f *HDLCFramer) {
		f.maxRetries = n
	}
}

// HDLCFramer implements Framer on top of an underlying io.ReadWriter.
// It uses internal/wire for framing and internal/arq for stop-and-wait ARQ.
type HDLCFramer struct {
	rw io.ReadWriter

	maxPayloadSize uint16
	autoFlush      bool

	sendBufMu sync.Mutex
	sendBuf   bytes.Buffer // buffered when autoFlush==false

	wireMu sync.Mutex // serialize writes to underlying rw

	// ARQ config & state
	retryInterval time.Duration
	maxRetries    int
	arq           *arq.StopAndWait

	// recv side
	recvMu     sync.Mutex
	recvBuf    bytes.Buffer // ordered bytes exposed via Read()
	recvCond   *sync.Cond
	expectedID uint16
	pending    map[uint16][]byte // buffered out-of-order payloads

	// lifecycle / errors
	closeOnce sync.Once
	closed    chan struct{}

	readErrMu sync.Mutex
	readErr   error
}

// frameSender adapts HDLCFramer to arq.Sender.
type frameSender struct {
	f *HDLCFramer
}

func (s *frameSender) SendFrame(id uint16, t wire.FrameType, payload []byte) error {
	return s.f.sendFrame(id, t, payload)
}

// NewFramer wraps an underlying io.ReadWriter with HDLC framing.
func NewFramer(rw io.ReadWriter, opts ...Option) *HDLCFramer {
	f := &HDLCFramer{
		rw:             rw,
		maxPayloadSize: DefaultMaxPayloadSize,
		retryInterval:  DefaultRetryInterval,
		maxRetries:     DefaultMaxRetries,
		closed:         make(chan struct{}),
		pending:        make(map[uint16][]byte),
		autoFlush:      true,
	}
	f.recvCond = sync.NewCond(&f.recvMu)

	for _, o := range opts {
		o(f)
	}

	f.arq = arq.NewStopAndWait(&frameSender{f: f}, f.retryInterval, f.maxRetries)

	go f.readLoop()

	return f
}

// SetAutoFlush changes the Write() behavior.
//
//   - If autoFlush is true (default), each call to Write() is immediately
//     segmented into frames and sent (with retries/acks).
//
//   - If autoFlush is false, Write() just buffers into an internal buffer
//     until Flush() is called or the buffer reaches MaxPayloadSize.
func (f *HDLCFramer) SetAutoFlush(b bool) {
	f.sendBufMu.Lock()
	defer f.sendBufMu.Unlock()

	if b && !f.autoFlush && f.sendBuf.Len() > 0 {
		_ = f.flushLocked()
	}
	f.autoFlush = b
}

// Flush forces buffered data to be framed and sent.
// Only relevant when autoFlush == false.
func (f *HDLCFramer) Flush() error {
	f.sendBufMu.Lock()
	defer f.sendBufMu.Unlock()
	return f.flushLocked()
}

func (f *HDLCFramer) flushLocked() error {
	for f.sendBuf.Len() > 0 {
		chunkSize := int(f.maxPayloadSize)
		if f.sendBuf.Len() < chunkSize {
			chunkSize = f.sendBuf.Len()
		}
		payload := make([]byte, chunkSize)
		_, _ = io.ReadFull(&f.sendBuf, payload)
		if err := f.sendDataFrame(payload); err != nil {
			return err
		}
	}
	return nil
}

// Read implements io.Reader. It returns data in-order, regardless of
// transient corruption or retransmission on the link.
func (f *HDLCFramer) Read(p []byte) (int, error) {
	f.recvMu.Lock()
	defer f.recvMu.Unlock()

	for f.recvBuf.Len() == 0 {
		if err := f.getReadErr(); err != nil {
			return 0, err
		}
		select {
		case <-f.closed:
			if f.recvBuf.Len() == 0 {
				if err := f.getReadErr(); err != nil {
					return 0, err
				}
				return 0, io.EOF
			}
		default:
		}
		f.recvCond.Wait()
	}

	return f.recvBuf.Read(p)
}

// Write implements io.Writer.
func (f *HDLCFramer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if f.autoFlush {
		offset := 0
		for offset < len(p) {
			chunkSize := int(f.maxPayloadSize)
			if len(p)-offset < chunkSize {
				chunkSize = len(p) - offset
			}
			chunk := make([]byte, chunkSize)
			copy(chunk, p[offset:offset+chunkSize])
			if err := f.sendDataFrame(chunk); err != nil {
				return offset, err
			}
			offset += chunkSize
		}
		return len(p), nil
	}

	// buffered mode
	f.sendBufMu.Lock()
	defer f.sendBufMu.Unlock()

	n, _ := f.sendBuf.Write(p)
	for f.sendBuf.Len() >= int(f.maxPayloadSize) {
		chunk := make([]byte, f.maxPayloadSize)
		_, _ = io.ReadFull(&f.sendBuf, chunk)
		if err := f.sendDataFrame(chunk); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Close stops background loops and propagates EOF/errors to readers/writers.
func (f *HDLCFramer) Close() error {
	f.closeOnce.Do(func() {
		if f.arq != nil {
			f.arq.Close()
		}
		close(f.closed)
		f.setReadErr(io.EOF)
		f.recvCond.Broadcast()
	})
	return nil
}

// ---------------- internal helpers ----------------

func (f *HDLCFramer) setReadErr(err error) {
	if err == nil {
		return
	}
	f.readErrMu.Lock()
	defer f.readErrMu.Unlock()
	if f.readErr == nil {
		f.readErr = err
	}
}

func (f *HDLCFramer) getReadErr() error {
	f.readErrMu.Lock()
	defer f.readErrMu.Unlock()
	return f.readErr
}

func (f *HDLCFramer) sendDataFrame(payload []byte) error {
	_, err := f.arq.Send(payload)
	return err
}

// sendFrame encodes and writes one frame to the underlying link.
func (f *HDLCFramer) sendFrame(id uint16, t wire.FrameType, payload []byte) error {
	if len(payload) > int(f.maxPayloadSize) {
		return errors.New("payload too large")
	}
	frame := wire.Frame{
		ID:      id,
		Type:    t,
		Payload: payload,
	}
	encoded, err := wire.EncodeFrame(frame)
	if err != nil {
		return err
	}

	f.wireMu.Lock()
	defer f.wireMu.Unlock()

	_, err = f.rw.Write(encoded)
	return err
}

func (f *HDLCFramer) sendAck(id uint16) {
	_ = f.sendFrame(id, wire.FrameTypeAck, nil)
}

func (f *HDLCFramer) sendNack(id uint16) {
	_ = f.sendFrame(id, wire.FrameTypeNack, nil)
}

// readLoop continuously reads frames from the underlying link.
func (f *HDLCFramer) readLoop() {
	for {
		select {
		case <-f.closed:
			return
		default:
		}

		raw, err := wire.ReadFrame(f.rw)
		if err != nil {
			f.setReadErr(err)
			_ = f.Close()
			return
		}
		if len(raw) == 0 {
			continue
		}

		frame, crcOK, err := wire.ParseFrame(raw)
		if err != nil {
			// malformed, drop
			continue
		}
		if !crcOK {
			// drop corrupted; rely on timeout-based retry. Optional NACK:
			f.sendNack(frame.ID)
			continue
		}

		switch frame.Type {
		case wire.FrameTypeData:
			f.handleDataFrame(frame.ID, frame.Payload)
			f.sendAck(frame.ID)
		case wire.FrameTypeAck:
			f.arq.HandleAck(frame.ID, false)
		case wire.FrameTypeNack:
			f.arq.HandleAck(frame.ID, true)
		}
	}
}

// handleDataFrame delivers ordered data to recvBuf and buffers out-of-order frames.
func (f *HDLCFramer) handleDataFrame(id uint16, payload []byte) {
	f.recvMu.Lock()
	defer f.recvMu.Unlock()

	if id < f.expectedID {
		return // duplicate/old
	}

	if id == f.expectedID {
		f.recvBuf.Write(payload)
		f.expectedID++
		for {
			p, ok := f.pending[f.expectedID]
			if !ok {
				break
			}
			f.recvBuf.Write(p)
			delete(f.pending, f.expectedID)
			f.expectedID++
		}
		f.recvCond.Signal()
		return
	}

	// out of order: buffer
	cp := make([]byte, len(payload))
	copy(cp, payload)
	f.pending[id] = cp
}
