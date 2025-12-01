// internal/arq/arq.go
package arq

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/sparques/hdlc/internal/wire"
)

const (
	DefaultRetryInterval = 200 * time.Millisecond
	DefaultMaxRetries    = 5
)

// Sender is implemented by something that can put a frame on the wire.
type Sender interface {
	SendFrame(id uint16, t wire.FrameType, payload []byte) error
}

// StopAndWait implements a simple stop-and-wait ARQ:
//
//   - Only one data frame may be outstanding at a time.
//   - Send() blocks until ACK/NACK or retry exhaustion.
type StopAndWait struct {
	sender        Sender
	retryInterval time.Duration
	maxRetries    int

	mu          sync.Mutex
	nextID      uint16
	outstanding *sentFrame

	closed    chan struct{}
	closeOnce sync.Once
}

type sentFrame struct {
	id       uint16
	payload  []byte
	retries  int
	lastSent time.Time
	done     chan error
}

// NewStopAndWait constructs a StopAndWait with the given Sender.
// retryInterval / maxRetries <= 0 mean "use defaults".
func NewStopAndWait(sender Sender, retryInterval time.Duration, maxRetries int) *StopAndWait {
	if retryInterval <= 0 {
		retryInterval = DefaultRetryInterval
	}
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	s := &StopAndWait{
		sender:        sender,
		retryInterval: retryInterval,
		maxRetries:    maxRetries,
		closed:        make(chan struct{}),
	}
	go s.retransmitLoop()
	return s
}

// Send sends one payload as a data frame, returning its assigned ID.
// Blocks until ACK/NACK or retry exhaustion/close.
func (s *StopAndWait) Send(payload []byte) (uint16, error) {
	sf := &sentFrame{
		payload: append([]byte(nil), payload...),
		done:    make(chan error, 1),
	}

	s.mu.Lock()
	if s.outstanding != nil {
		s.mu.Unlock()
		return 0, errors.New("outstanding frame already in flight")
	}
	sf.id = s.nextID
	s.nextID++
	s.outstanding = sf
	s.mu.Unlock()

	if err := s.sender.SendFrame(sf.id, wire.FrameTypeData, sf.payload); err != nil {
		s.mu.Lock()
		if s.outstanding == sf {
			s.outstanding = nil
		}
		s.mu.Unlock()
		return sf.id, err
	}
	sf.lastSent = time.Now()

	select {
	case err := <-sf.done:
		s.mu.Lock()
		if s.outstanding == sf {
			s.outstanding = nil
		}
		s.mu.Unlock()
		return sf.id, err
	case <-s.closed:
		s.mu.Lock()
		if s.outstanding == sf {
			s.outstanding = nil
		}
		s.mu.Unlock()
		return sf.id, io.EOF
	}
}

// HandleAck should be called by the owner when an ACK or NACK
// is received for a given frame ID.
func (s *StopAndWait) HandleAck(id uint16, isNack bool) {
	s.mu.Lock()
	sf := s.outstanding
	s.mu.Unlock()

	if sf == nil || sf.id != id {
		return
	}

	if isNack {
		// Treat NACK as immediate timeout; let retransmitLoop handle retries,
		// but bump retries here and maybe fail fast.
		if sf.retries >= s.maxRetries {
			sf.done <- errors.New("max retries exceeded (nack)")
			return
		}
		sf.retries++
		sf.lastSent = time.Now()
		_ = s.sender.SendFrame(sf.id, wire.FrameTypeData, sf.payload)
		return
	}

	// ACK
	sf.done <- nil
}

// Close stops the retransmit loop and unblocks any Send.
func (s *StopAndWait) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)

		s.mu.Lock()
		defer s.mu.Unlock()
		if s.outstanding != nil {
			s.outstanding.done <- io.EOF
			s.outstanding = nil
		}
	})
}

func (s *StopAndWait) retransmitLoop() {
	ticker := time.NewTicker(s.retryInterval / 2)
	defer ticker.Stop()

	for {
		select {
		case <-s.closed:
			return
		case <-ticker.C:
		}

		s.mu.Lock()
		sf := s.outstanding
		s.mu.Unlock()
		if sf == nil {
			continue
		}

		if time.Since(sf.lastSent) >= s.retryInterval {
			if sf.retries >= s.maxRetries {
				sf.done <- errors.New("max retries exceeded")
				continue
			}
			sf.retries++
			sf.lastSent = time.Now()
			_ = s.sender.SendFrame(sf.id, wire.FrameTypeData, sf.payload)
		}
	}
}
