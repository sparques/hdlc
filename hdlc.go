// Package hdlc implements an HDLC/SLIP-style framing layer with CRC and retries
// on top of an unreliable byte stream (e.g. UART).
//
// Wire format (after SLIP-style byte stuffing):
//
//	FLAG (0x7E)
//	[stuffed bytes of]
//	  frameID   : uint16 BE
//	  length    : uint16 BE
//	  frameType : uint8  (0=data, 1=ack, 2=nack)
//	  payload   : length bytes
//	  crc32     : uint32 BE over (frameID|length|frameType|payload)
//	FLAG (0x7E)
//
// All occurrences of FLAG (0x7E) and ESC (0x7D) in the inner bytes
// are escaped as: ESC, byte^0x20.
//
// The public type HDLCFramer implements:
//
//	type Framer interface {
//	    SetAutoFlush(bool)
//	    io.ReadWriter
//	}
//
// HDLCFramer also provides a Flush() method when autoFlush is false.
package hdlc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"sync"
	"time"
)

const (
	flagByte = 0x7E
	escByte  = 0x7D
	escXor   = 0x20

	frameTypeData = 0x00
	frameTypeAck  = 0x01
	frameTypeNack = 0x02

	DefaultMaxPayloadSize = 0xFFFF // uint16 max
	DefaultRetryInterval  = 200 * time.Millisecond
	DefaultMaxRetries     = 5
)

// Koopman CRC32 polynomial.
const koopmanPoly = 0x741B8CD7

var koopmanTable = crc32.MakeTable(koopmanPoly)

// Framer is an interface that wraps io.ReadWriter
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
//
// It spawns goroutines internally:
//   - readLoop: decodes frames, checks CRC, delivers data, handles acks.
//   - retransmitLoop: resends timed-out frames.
//
// It uses stop-and-wait ARQ: only one data frame is outstanding at a time.
type HDLCFramer struct {
	rw io.ReadWriter

	// framing / sending
	maxPayloadSize uint16
	autoFlush      bool

	sendBufMu sync.Mutex
	sendBuf   bytes.Buffer // buffered when autoFlush==false

	wireMu sync.Mutex // serialize writes to underlying rw

	// ARQ
	retryInterval time.Duration
	maxRetries    int

	sentMu sync.Mutex
	sent   *sentFrame // single outstanding frame (stop-and-wait)

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

	// sequence numbers
	nextIDMu sync.Mutex
	nextID   uint16
}

// sentFrame tracks one outstanding data frame waiting for ack.
type sentFrame struct {
	id       uint16
	payload  []byte
	retries  int
	lastSent time.Time
	done     chan error
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
	}
	f.recvCond = sync.NewCond(&f.recvMu)

	for _, o := range opts {
		o(f)
	}

	// start background workers
	go f.readLoop()
	go f.retransmitLoop()

	return f
}

// SetAutoFlush changes the Write() behavior.
//
//   - If autoFlush is true (default), each call to Write() is immediately
//     segmented into frames and sent (with retries/acks).
//
//   - If autoFlush is false, Write() just buffers into an internal buffer
//     until Flush() is called or the buffer reaches MaxPayloadSize, at which
//     point frames are emitted.
func (f *HDLCFramer) SetAutoFlush(b bool) {
	f.sendBufMu.Lock()
	defer f.sendBufMu.Unlock()

	if b && !f.autoFlush && f.sendBuf.Len() > 0 {
		// flush what we have when switching from buffered to autoflush.
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
		// immediate framing
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
// Not part of the Framer interface, but handy.
func (f *HDLCFramer) Close() error {
	f.closeOnce.Do(func() {
		close(f.closed)
		f.setReadErr(io.EOF)
		f.recvCond.Broadcast()
	})
	return nil
}

// ---------------- internal helpers ----------------

func (f *HDLCFramer) getNextID() uint16 {
	f.nextIDMu.Lock()
	defer f.nextIDMu.Unlock()
	id := f.nextID
	f.nextID++
	return id
}

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

// sendDataFrame sends one data frame and waits for its ACK (with retries).
func (f *HDLCFramer) sendDataFrame(payload []byte) error {
	sf := &sentFrame{
		id:      f.getNextID(),
		payload: payload,
		done:    make(chan error, 1),
	}

	f.sentMu.Lock()
	if f.sent != nil {
		// stop-and-wait: should never happen if we correctly wait on done
		f.sentMu.Unlock()
		return errors.New("internal: sendDataFrame called with outstanding frame")
	}
	f.sent = sf
	f.sentMu.Unlock()

	if err := f.sendFrame(sf.id, frameTypeData, sf.payload); err != nil {
		f.sentMu.Lock()
		f.sent = nil
		f.sentMu.Unlock()
		return err
	}
	sf.lastSent = time.Now()

	// wait for ack / error
	select {
	case err := <-sf.done:
		f.sentMu.Lock()
		f.sent = nil
		f.sentMu.Unlock()
		return err
	case <-f.closed:
		return io.EOF
	}
}

// sendFrame encodes and writes one frame to the underlying link.
func (f *HDLCFramer) sendFrame(id uint16, frameType byte, payload []byte) error {
	headerLen := 2 + 2 + 1 // id(2) + len(2) + type(1)
	if len(payload) > int(f.maxPayloadSize) {
		return errors.New("payload too large")
	}

	raw := make([]byte, headerLen+len(payload))
	binary.BigEndian.PutUint16(raw[0:2], id)
	binary.BigEndian.PutUint16(raw[2:4], uint16(len(payload)))
	raw[4] = frameType
	copy(raw[5:], payload)

	crc := crc32.Checksum(raw, koopmanTable)
	withCRC := make([]byte, len(raw)+4)
	copy(withCRC, raw)
	binary.BigEndian.PutUint32(withCRC[len(raw):], crc)

	stuffed := slipStuff(withCRC)

	frame := make([]byte, 0, len(stuffed)+2)
	frame = append(frame, flagByte)
	frame = append(frame, stuffed...)
	frame = append(frame, flagByte)

	f.wireMu.Lock()
	defer f.wireMu.Unlock()

	_, err := f.rw.Write(frame)
	return err
}

// sendAck/Nack do not participate in ARQ themselves.
func (f *HDLCFramer) sendAck(id uint16) {
	_ = f.sendFrame(id, frameTypeAck, nil)
}

func (f *HDLCFramer) sendNack(id uint16) {
	_ = f.sendFrame(id, frameTypeNack, nil)
}

// readLoop continuously reads frames from the underlying link.
func (f *HDLCFramer) readLoop() {
	for {
		select {
		case <-f.closed:
			return
		default:
		}

		data, err := f.readOneFrame()
		if err != nil {
			f.setReadErr(err)
			f.recvCond.Broadcast()
			return
		}
		if len(data) == 0 {
			continue
		}
		id, payload, frameType, crcOK, err := parseFrame(data)
		if err != nil {
			// malformed, drop
			continue
		}
		if !crcOK {
			// Bad CRC. We drop and rely on timeout; optional NACK:
			f.sendNack(id)
			continue
		}

		switch frameType {
		case frameTypeData:
			f.handleDataFrame(id, payload)
			f.sendAck(id)
		case frameTypeAck:
			f.handleAck(id, false)
		case frameTypeNack:
			f.handleAck(id, true)
		}
	}
}

// retransmitLoop periodically checks the outstanding frame and resends on timeout.
func (f *HDLCFramer) retransmitLoop() {
	ticker := time.NewTicker(f.retryInterval / 2)
	defer ticker.Stop()

	for {
		select {
		case <-f.closed:
			return
		case <-ticker.C:
		}

		f.sentMu.Lock()
		sf := f.sent
		f.sentMu.Unlock()
		if sf == nil {
			continue
		}

		if time.Since(sf.lastSent) >= f.retryInterval {
			if sf.retries >= f.maxRetries {
				sf.done <- errors.New("max retries exceeded")
				// clearing f.sent is done in sendDataFrame after done
				continue
			}
			sf.retries++
			sf.lastSent = time.Now()
			_ = f.sendFrame(sf.id, frameTypeData, sf.payload)
		}
	}
}

// handleDataFrame delivers ordered data to recvBuf and buffers out-of-order frames.
// (With stop-and-wait we should not see out-of-order,
// but this makes it easy to extend to sliding windows later.)
func (f *HDLCFramer) handleDataFrame(id uint16, payload []byte) {
	f.recvMu.Lock()
	defer f.recvMu.Unlock()

	// duplicate or old frame
	if id < f.expectedID {
		return
	}

	if id == f.expectedID {
		f.recvBuf.Write(payload)
		f.expectedID++
		// flush any buffered successors
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

// handleAck processes ACK/NACK for the outstanding frame.
func (f *HDLCFramer) handleAck(id uint16, isNack bool) {
	f.sentMu.Lock()
	sf := f.sent
	f.sentMu.Unlock()

	if sf == nil || sf.id != id {
		return
	}

	if isNack {
		// Treat NACK as immediate timeout; retransmit or fail.
		if sf.retries >= f.maxRetries {
			sf.done <- errors.New("max retries exceeded (nack)")
		} else {
			sf.retries++
			sf.lastSent = time.Now()
			_ = f.sendFrame(sf.id, frameTypeData, sf.payload)
		}
		return
	}

	// ACK
	sf.done <- nil
}

// readOneFrame reads until a full frame (FLAG..FLAG) is obtained and un-stuffed.
func (f *HDLCFramer) readOneFrame() ([]byte, error) {
	var buf []byte
	inFrame := false
	tmp := []byte{0}

	for {
		n, err := f.rw.Read(tmp)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		b := tmp[0]

		if !inFrame {
			if b == flagByte {
				inFrame = true
				buf = buf[:0]
			}
			continue
		}

		if b == flagByte {
			// end of frame
			if len(buf) == 0 {
				// empty frame, ignore and resync
				inFrame = false
				continue
			}
			return slipUnstuff(buf)
		}

		buf = append(buf, b)
	}
}

// parseFrame splits an unstuffed frame into header, payload, and CRC.
func parseFrame(data []byte) (id uint16, payload []byte, frameType byte, crcOK bool, err error) {
	if len(data) < 2+2+1+4 {
		err = errors.New("frame too short")
		return
	}
	id = binary.BigEndian.Uint16(data[0:2])
	length := binary.BigEndian.Uint16(data[2:4])
	frameType = data[4]

	headerAndPayloadLen := 2 + 2 + 1 + int(length) // 5+len
	if len(data) < headerAndPayloadLen+4 {
		err = errors.New("incomplete frame")
		return
	}

	headerAndPayload := data[:headerAndPayloadLen]
	crcBytes := data[headerAndPayloadLen : headerAndPayloadLen+4]
	expectedCRC := binary.BigEndian.Uint32(crcBytes)
	actualCRC := crc32.Checksum(headerAndPayload, koopmanTable)
	crcOK = (expectedCRC == actualCRC)

	payload = headerAndPayload[5:]
	return
}

// slipStuff performs SLIP-style escaping on a byte slice.
func slipStuff(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for _, b := range in {
		if b == flagByte || b == escByte {
			out = append(out, escByte, b^escXor)
		} else {
			out = append(out, b)
		}
	}
	return out
}

// slipUnstuff reverses slipStuff.
func slipUnstuff(in []byte) ([]byte, error) {
	out := make([]byte, 0, len(in))
	esc := false
	for _, b := range in {
		if esc {
			out = append(out, b^escXor)
			esc = false
			continue
		}
		if b == escByte {
			esc = true
			continue
		}
		out = append(out, b)
	}
	if esc {
		return nil, errors.New("trailing escape byte")
	}
	return out, nil
}
