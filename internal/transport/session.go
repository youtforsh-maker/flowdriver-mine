package transport

import (
	"sync"
	"time"
)

// Direction indicates if a file is req (client to server) or res (server to client)
type Direction string

const (
	DirReq Direction = "req"
	DirRes Direction = "res"
)

// Session represents an active proxy connection mapped to files.
type Session struct {
	ID           string
	mu           sync.Mutex
	txBuf        []byte
	txSeq        uint64
	rxSeq        uint64
	rxQueue      map[uint64]*Envelope
	lastActivity time.Time
	closed       bool
	rxClosed     bool
	TargetAddr   string
	ClientID     string

	txCond *sync.Cond
	RxChan chan []byte
}

func NewSession(id string) *Session {
	s := &Session{
		ID:           id,
		txBuf:        make([]byte, 0, 32*1024), // OPT: pre-alloc 32KB to avoid early realloc
		rxQueue:      make(map[uint64]*Envelope),
		lastActivity: time.Now(),
		RxChan:       make(chan []byte, 1024),
	}
	s.txCond = sync.NewCond(&s.mu)
	return s
}

func (s *Session) EnqueueTx(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for len(s.txBuf) > 2*1024*1024 && !s.closed {
		s.txCond.Wait()
	}

	s.txBuf = append(s.txBuf, data...)
	s.lastActivity = time.Now()
}

func (s *Session) ClearTx() {
	s.mu.Lock()
	s.txBuf = nil
	s.txCond.Broadcast()
	s.mu.Unlock()
}

// ProcessRx handles an incoming envelope from the remote side.
//
// OPT-01 FIX — the original held s.mu for the entire function, including
// the `s.RxChan <- payload` channel send. When RxChan is full (application
// reader is slower than the network), the goroutine blocks indefinitely
// while holding s.mu. This cascades to:
//
//   - EnqueueTx (server's TCP read loop): blocked → server stops reading from
//     the real TCP connection → TCP receive window fills → remote server slows.
//   - flushAll (every 300ms): blocked → txBuf can't be drained → no TX data
//     uploaded to Drive while the stall persists.
//   - Sequential mux decode: ALL other sessions' envelopes in the same mux
//     file are delayed by session A's backpressure, even if session B's
//     RxChan is completely empty.
//
// The fix: collect everything under the lock (nanoseconds of pure memory
// work), release the lock, THEN do the channel sends. The mutex is never held
// during any blocking I/O.
func (s *Session) ProcessRx(env *Envelope) {
	s.mu.Lock()

	if s.rxClosed {
		s.mu.Unlock()
		return
	}

	// Collect payloads and determine close intent — all under lock.
	var toDispatch [][]byte
	var shouldClose bool

	if env.Seq == s.rxSeq {
		if len(env.Payload) > 0 {
			toDispatch = append(toDispatch, env.Payload)
		}
		s.rxSeq++
		shouldClose = env.Close

		// Drain any out-of-order packets that are now in sequence.
		for !shouldClose {
			next, ok := s.rxQueue[s.rxSeq]
			if !ok {
				break
			}
			if len(next.Payload) > 0 {
				toDispatch = append(toDispatch, next.Payload)
			}
			delete(s.rxQueue, s.rxSeq)
			s.rxSeq++
			shouldClose = next.Close
		}

		if shouldClose {
			// Set under lock so RemoveSession sees rxClosed=true and won't
			// attempt a concurrent close(RxChan), preventing a double-close panic.
			s.rxClosed = true
			s.closed = true
		}
	} else if env.Seq > s.rxSeq {
		s.rxQueue[env.Seq] = env
	}
	// env.Seq < s.rxSeq → duplicate already delivered, discard silently.

	s.lastActivity = time.Now()
	s.mu.Unlock() // ← RELEASED before any channel operation

	// These sends can block if the app reader is slow.
	// s.mu is free, so EnqueueTx, flushAll, and other sessions are unaffected.
	for _, payload := range toDispatch {
		s.RxChan <- payload
	}

	if shouldClose {
		close(s.RxChan)
	}
}
