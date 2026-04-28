package transport

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestEnvelopeMarshalBinaryRoundTrip(t *testing.T) {
	original := &Envelope{
		SessionID:  "session-123",
		Seq:        42,
		TargetAddr: "example.com:443",
		Payload:    []byte("payload-data"),
		Close:      true,
	}

	encoded, err := original.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary() error = %v", err)
	}

	var decoded Envelope
	n, err := decoded.UnmarshalBinary(encoded)
	if err != nil {
		t.Fatalf("UnmarshalBinary() error = %v", err)
	}
	if n != len(encoded) {
		t.Fatalf("UnmarshalBinary() bytes read = %d, want %d", n, len(encoded))
	}

	assertEnvelopeEqual(t, &decoded, original)
}

func TestEnvelopeEncodeDecodeRoundTrip(t *testing.T) {
	original := &Envelope{
		SessionID:  "abc",
		Seq:        7,
		TargetAddr: "127.0.0.1:8080",
		Payload:    []byte{1, 2, 3, 4, 5},
		Close:      false,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	var decoded Envelope
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	assertEnvelopeEqual(t, &decoded, original)
}

func TestEnvelopeDecodeTruncatedPayload(t *testing.T) {
	original := &Envelope{
		SessionID:  "trunc",
		Seq:        1,
		TargetAddr: "host:80",
		Payload:    []byte("hello"),
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	data := buf.Bytes()
	truncated := data[:len(data)-2]

	var decoded Envelope
	err := decoded.Decode(bytes.NewReader(truncated))
	if err == nil {
		t.Fatal("Decode() error = nil, want io.ErrUnexpectedEOF")
	}
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("Decode() error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
}

func TestEnvelopeDecodeRejectsOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(MagicByte)
	buf.WriteByte(byte(len("sid")))
	buf.WriteString("sid")
	if err := binary.Write(&buf, binary.BigEndian, uint64(9)); err != nil {
		t.Fatalf("binary.Write(seq) error = %v", err)
	}
	buf.WriteByte(byte(len("host:443")))
	buf.WriteString("host:443")
	buf.WriteByte(0)
	if err := binary.Write(&buf, binary.BigEndian, uint32(10*1024*1024+1)); err != nil {
		t.Fatalf("binary.Write(payloadLen) error = %v", err)
	}

	var decoded Envelope
	err := decoded.Decode(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("Decode() error = nil, want oversized packet error")
	}
	if got := err.Error(); got != "packet too large: 10485761" {
		t.Fatalf("Decode() error = %q, want oversized packet error", got)
	}
}

func assertEnvelopeEqual(t *testing.T, got, want *Envelope) {
	t.Helper()

	if got.SessionID != want.SessionID {
		t.Fatalf("SessionID = %q, want %q", got.SessionID, want.SessionID)
	}
	if got.Seq != want.Seq {
		t.Fatalf("Seq = %d, want %d", got.Seq, want.Seq)
	}
	if got.TargetAddr != want.TargetAddr {
		t.Fatalf("TargetAddr = %q, want %q", got.TargetAddr, want.TargetAddr)
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("Payload = %v, want %v", got.Payload, want.Payload)
	}
	if got.Close != want.Close {
		t.Fatalf("Close = %v, want %v", got.Close, want.Close)
	}
}
