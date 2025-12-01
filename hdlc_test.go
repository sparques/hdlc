// hdlc/hdlc_test.go
package hdlc

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// Test that bufio.Reader line-based code works transparently on top of HDLCFramer
// even though frames may cut lines arbitrarily.
func TestBufioLineReaderOverFramer(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	fw := NewFramer(c1, WithRetryInterval(50*time.Millisecond), WithMaxRetries(3))
	fr := NewFramer(c2, WithRetryInterval(50*time.Millisecond), WithMaxRetries(3))
	defer fw.Close()
	defer fr.Close()

	const nLines = 50

	// writer side: send N lines
	doneWriter := make(chan struct{})
	go func() {
		defer close(doneWriter)
		for i := 0; i < nLines; i++ {
			line := fmt.Sprintf("line-%03d: hello world\n", i)
			// Purposely sometimes send partials to stress fragmentation.
			if i%2 == 0 {
				if _, err := fw.Write([]byte(line)); err != nil {
					return
				}
			} else {
				// split into two writes
				mid := len(line) / 2
				if _, err := fw.Write([]byte(line[:mid])); err != nil {
					return
				}
				if _, err := fw.Write([]byte(line[mid:])); err != nil {
					return
				}
			}
		}
	}()

	// reader side: use bufio.Reader to read line by line.
	br := bufio.NewReader(fr)

	for i := 0; i < nLines; i++ {
		want := fmt.Sprintf("line-%03d: hello world\n", i)
		got, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString error at line %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("line %d mismatch: got %q, want %q", i, got, want)
		}
	}

	<-doneWriter
}

// Quick sanity: a big payload is split into multiple frames but reassembled
// into a contiguous byte stream.
func TestLargeWriteSplitAndReassemble(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	fw := NewFramer(c1, WithMaxPayloadSize(64)) // tiny to force many frames
	fr := NewFramer(c2)
	defer fw.Close()
	defer fr.Close()

	orig := strings.Repeat("ABCDEFGHIJKLMNOPQRSTUVWXYZ", 100) // 2600 bytes

	go func() {
		defer fw.Close()
		if _, err := fw.Write([]byte(orig)); err != nil {
			t.Logf("writer error: %v", err)
		}
	}()

	buf := make([]byte, len(orig))
	n, err := ioReadFull(fr, buf)
	if err != nil {
		t.Fatalf("ReadFull error: %v", err)
	}
	if n != len(orig) {
		t.Fatalf("read %d bytes, want %d", n, len(orig))
	}
	if string(buf) != orig {
		t.Fatalf("payload mismatch")
	}
}

// small helper to avoid importing io in tests just for ReadFull
func ioReadFull(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}
