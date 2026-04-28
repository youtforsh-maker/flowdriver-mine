package transport

import (
	"io"
	"net"
	"time"
)

// VirtualConn implements net.Conn routing data through a Session.
type VirtualConn struct {
	session *Session
	engine  *Engine
	readBuf []byte
	// No mutex needed: Read() is called from exactly one goroutine (the SOCKS5
	// relay goroutine assigned to this connection). readBuf is only touched
	// inside Read(). Write() and Close() operate on session fields that carry
	// their own locks (session.mu, txCond).
}

func NewVirtualConn(s *Session, e *Engine) *VirtualConn {
	return &VirtualConn{session: s, engine: e}
}

func (v *VirtualConn) Read(b []byte) (n int, err error) {
	for {
		// Fast path: bytes left over from a previous partial copy.
		if len(v.readBuf) > 0 {
			n = copy(b, v.readBuf)
			v.readBuf = v.readBuf[n:]
			return n, nil
		}

		// Block until the engine delivers data or the session closes.
		// When RemoveSession() calls close(s.RxChan), ok=false → io.EOF,
		// which unblocks this goroutine instead of hanging forever.
		data, ok := <-v.session.RxChan
		if !ok {
			return 0, io.EOF
		}
		if len(data) == 0 {
			v.session.mu.Lock()
			closed := v.session.closed
			v.session.mu.Unlock()
			if closed {
				return 0, io.EOF
			}
			continue
		}

		n = copy(b, data)
		if n < len(data) {
			v.readBuf = data[n:]
		}
		return n, nil
	}
}

func (v *VirtualConn) Write(b []byte) (n int, err error) {
	if len(b) > 0 {
		v.session.EnqueueTx(b)
		// Wake the flush loop immediately so data is uploaded to Drive now,
		// not after the next flushTicker tick (up to 300ms later).
		v.engine.SignalFlush()
	}
	return len(b), nil
}

func (v *VirtualConn) Close() error {
	v.session.mu.Lock()
	v.session.closed = true
	v.session.txCond.Broadcast()
	v.session.mu.Unlock()
	return nil
}

func (v *VirtualConn) LocalAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (v *VirtualConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (v *VirtualConn) SetDeadline(t time.Time) error      { return nil }
func (v *VirtualConn) SetReadDeadline(t time.Time) error  { return nil }
func (v *VirtualConn) SetWriteDeadline(t time.Time) error { return nil }
