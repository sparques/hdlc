// internal/wire/wire.go
package wire

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

const (
	Flag   byte = 0x7E
	Esc    byte = 0x7D
	EscXor byte = 0x20
)

// FrameType identifies frame purpose on the link.
type FrameType byte

const (
	FrameTypeData FrameType = 0x00
	FrameTypeAck  FrameType = 0x01
	FrameTypeNack FrameType = 0x02
)

type Frame struct {
	ID      uint16
	Type    FrameType
	Payload []byte
}

// Koopman CRC32 polynomial.
const KoopmanPoly = 0x741B8CD7

var koopmanTable = crc32.MakeTable(KoopmanPoly)

// EncodeFrame builds a full on-wire frame including flags and SLIP-style
// escaping. Payload length is encoded as uint16 (big-endian).
func EncodeFrame(f Frame) ([]byte, error) {
	if len(f.Payload) > 0xFFFF {
		return nil, errors.New("payload too large for uint16 length")
	}

	headerLen := 2 + 2 + 1 // id + len + type
	raw := make([]byte, headerLen+len(f.Payload))
	binary.BigEndian.PutUint16(raw[0:2], f.ID)
	binary.BigEndian.PutUint16(raw[2:4], uint16(len(f.Payload)))
	raw[4] = byte(f.Type)
	copy(raw[5:], f.Payload)

	crc := crc32.Checksum(raw, koopmanTable)
	withCRC := make([]byte, len(raw)+4)
	copy(withCRC, raw)
	binary.BigEndian.PutUint32(withCRC[len(raw):], crc)

	stuffed := slipStuff(withCRC)

	out := make([]byte, 0, len(stuffed)+2)
	out = append(out, Flag)
	out = append(out, stuffed...)
	out = append(out, Flag)
	return out, nil
}

// ReadFrame reads one frame (Flag .. Flag) from r, un-stuffs it,
// and returns the inner bytes (header+payload+crc). It resynchronizes
// on Flag boundaries.
func ReadFrame(r io.Reader) ([]byte, error) {
	var buf []byte
	inFrame := false
	tmp := []byte{0}

	for {
		n, err := r.Read(tmp)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		b := tmp[0]

		if !inFrame {
			if b == Flag {
				inFrame = true
				buf = buf[:0]
			}
			continue
		}

		if b == Flag {
			if len(buf) == 0 {
				// ignore empty frame
				inFrame = false
				continue
			}
			return slipUnstuff(buf)
		}

		buf = append(buf, b)
	}
}

// ParseFrame decodes a raw frame (header+payload+crc) into a Frame.
// Returns (frame, crcOK, error). If error != nil, frame is undefined.
func ParseFrame(data []byte) (Frame, bool, error) {
	var f Frame

	if len(data) < 2+2+1+4 {
		return f, false, errors.New("frame too short")
	}

	id := binary.BigEndian.Uint16(data[0:2])
	length := binary.BigEndian.Uint16(data[2:4])
	ft := FrameType(data[4])

	headerAndPayloadLen := 2 + 2 + 1 + int(length)
	if len(data) < headerAndPayloadLen+4 {
		return f, false, errors.New("incomplete frame")
	}

	headerAndPayload := data[:headerAndPayloadLen]
	crcBytes := data[headerAndPayloadLen : headerAndPayloadLen+4]
	expectedCRC := binary.BigEndian.Uint32(crcBytes)
	actualCRC := crc32.Checksum(headerAndPayload, koopmanTable)
	crcOK := (expectedCRC == actualCRC)

	payload := headerAndPayload[5:]

	f.ID = id
	f.Type = ft
	f.Payload = payload

	return f, crcOK, nil
}

// slipStuff performs SLIP-style escaping on a byte slice.
func slipStuff(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for _, b := range in {
		if b == Flag || b == Esc {
			out = append(out, Esc, b^EscXor)
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
			out = append(out, b^EscXor)
			esc = false
			continue
		}
		if b == Esc {
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
