package transport

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Envelope represents a message exchanged between the client and server.
type Envelope struct {
	// SessionID is the unique identifier for the connection (UUID).
	SessionID string `json:"session_id"`

	// Seq is the sequence number for ordering packets.
	Seq uint64 `json:"seq"`

	// TargetAddr is used by the client on the first sequence to tell the server where to connect.
	TargetAddr string `json:"target_addr,omitempty"`

	// Payload contains the actual application data.
	Payload []byte `json:"payload,omitempty"`

	// Close implies that the sender is closing its write side of the session.
	Close bool `json:"close,omitempty"`
}

const (
	MagicByte = 0x1F
)

// MarshalBinary serializes the envelope into the custom Flow binary format.
func (e *Envelope) MarshalBinary() ([]byte, error) {
	totalSize := 1 + 1 + len(e.SessionID) + 8 + 1 + len(e.TargetAddr) + 1 + 4 + len(e.Payload)
	buf := make([]byte, totalSize)
	
	buf[0] = MagicByte
	buf[1] = uint8(len(e.SessionID))
	offset := 2
	copy(buf[offset:], e.SessionID)
	offset += len(e.SessionID)
	
	binary.BigEndian.PutUint64(buf[offset:], e.Seq)
	offset += 8
	
	buf[offset] = uint8(len(e.TargetAddr))
	offset++
	copy(buf[offset:], e.TargetAddr)
	offset += len(e.TargetAddr)
	
	if e.Close {
		buf[offset] = 1
	} else {
		buf[offset] = 0
	}
	offset++
	
	binary.BigEndian.PutUint32(buf[offset:], uint32(len(e.Payload)))
	offset += 4
	
	copy(buf[offset:], e.Payload)
	return buf, nil
}

// Encode writes the envelope directly to an io.Writer.
func (e *Envelope) Encode(w io.Writer) error {
	// Max header: 1 (magic) + 1 (sidLen) + 255 (SID) + 8 (seq) + 1 (addrLen) + 255 (addr) + 1 (close) + 4 (payLen) = 526
	var hdr [530]byte
	hdr[0] = MagicByte
	hdr[1] = uint8(len(e.SessionID))
	copy(hdr[2:], e.SessionID)
	offset := 2 + len(e.SessionID)

	binary.BigEndian.PutUint64(hdr[offset:], e.Seq)
	offset += 8

	hdr[offset] = uint8(len(e.TargetAddr))
	offset++
	copy(hdr[offset:], e.TargetAddr)
	offset += len(e.TargetAddr)

	if e.Close {
		hdr[offset] = 1
	} else {
		hdr[offset] = 0
	}
	offset++

	binary.BigEndian.PutUint32(hdr[offset:], uint32(len(e.Payload)))
	offset += 4

	if _, err := w.Write(hdr[:offset]); err != nil {
		return err
	}
	if len(e.Payload) > 0 {
		_, err := w.Write(e.Payload)
		return err
	}
	return nil
}

// UnmarshalBinary deserializes the envelope from the custom Flow binary format.
// It returns the number of bytes read or an error.
func (e *Envelope) UnmarshalBinary(data []byte) (int, error) {
	if len(data) < 1 {
		return 0, io.ErrUnexpectedEOF
	}
	if data[0] != MagicByte {
		return 0, fmt.Errorf("invalid magic byte: expected 0x%X, got 0x%X", MagicByte, data[0])
	}
	
	offset := 1
	if len(data) < offset+1 { return 0, io.ErrUnexpectedEOF }
	sidLen := int(data[offset])
	offset++
	
	if len(data) < offset+sidLen { return 0, io.ErrUnexpectedEOF }
	e.SessionID = string(data[offset : offset+sidLen])
	offset += sidLen
	
	if len(data) < offset+8 { return 0, io.ErrUnexpectedEOF }
	e.Seq = binary.BigEndian.Uint64(data[offset:])
	offset += 8
	
	if len(data) < offset+1 { return 0, io.ErrUnexpectedEOF }
	addrLen := int(data[offset])
	offset++
	
	if len(data) < offset+addrLen { return 0, io.ErrUnexpectedEOF }
	e.TargetAddr = string(data[offset : offset+addrLen])
	offset += addrLen
	
	if len(data) < offset+1 { return 0, io.ErrUnexpectedEOF }
	e.Close = data[offset] == 1
	offset++
	
	if len(data) < offset+4 { return 0, io.ErrUnexpectedEOF }
	payloadLen := int(binary.BigEndian.Uint32(data[offset:]))
	offset += 4
	
	if len(data) < offset+payloadLen { return 0, io.ErrUnexpectedEOF }
	e.Payload = make([]byte, payloadLen)
	copy(e.Payload, data[offset:offset+payloadLen])
	offset += payloadLen
	
	return offset, nil
}
// Decode reads an envelope from an io.Reader.
// OPT: Uses a single stack-allocated buffer for session ID and target addr,
// eliminating 2 heap allocations per envelope decode.
func (e *Envelope) Decode(r io.Reader) error {
	// Read magic byte + session ID length in one call.
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	if hdr[0] != MagicByte {
		return fmt.Errorf("invalid magic byte: 0x%X", hdr[0])
	}
	sidLen := int(hdr[1])

	// OPT: Stack-allocated buffer for sessionID + seq(8) + addrLen(1).
	// Avoids heap allocation for the session ID read.
	// Max sidLen is 255 (uint8); 255 + 9 = 264 fits in 272.
	var sidAndMeta [272]byte
	need := sidLen + 9 // sessionID bytes + 8 (seq) + 1 (addrLen)
	if _, err := io.ReadFull(r, sidAndMeta[:need]); err != nil {
		return err
	}
	e.SessionID = string(sidAndMeta[:sidLen])
	e.Seq = binary.BigEndian.Uint64(sidAndMeta[sidLen : sidLen+8])
	addrLen := int(sidAndMeta[sidLen+8])

	// OPT: Stack-allocated buffer for targetAddr + close(1) + payLen(4).
	var addrAndTail [261]byte // max addrLen(255) + 5
	need2 := addrLen + 5
	if _, err := io.ReadFull(r, addrAndTail[:need2]); err != nil {
		return err
	}
	e.TargetAddr = string(addrAndTail[:addrLen])
	e.Close = addrAndTail[addrLen] == 1

	payLen := binary.BigEndian.Uint32(addrAndTail[addrLen+1 : addrLen+5])
	if payLen > 10*1024*1024 { // Sanity check: 10MB max packet
		return fmt.Errorf("packet too large: %d", payLen)
	}
	if payLen > 0 {
		e.Payload = make([]byte, payLen)
		if _, err := io.ReadFull(r, e.Payload); err != nil {
			return err
		}
	} else {
		e.Payload = nil
	}
	return nil
}
