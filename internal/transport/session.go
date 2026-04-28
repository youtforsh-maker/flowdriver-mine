package transport

import (
	"log"
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

	// OPT-D5: Maximum number of out-of-order envelopes to buffer.
	// Prevents unbounded memory growth if packets arrive wildly out-of-order.
	MaxRxQueueSize int
}

// DefaultRxChanSize is the default buffered channel size for incoming payloads.
const DefaultRxChanSize = 1024

// DefaultMaxRxQueueSize caps the out-of-order resequencing buffer.
// 10000 entries × ~1KB avg payload ≈ 10MB max memory per session.
const DefaultMaxRxQueueSize = 10000

func NewSession(id string) *Session {
	return NewSessionWithChanSize(id, DefaultRxChanSize)
}

// NewSessionWithChanSize creates a session with a custom RxChan buffer size.
// OPT-D2: Allows tuning via config for different workload profiles.
func NewSessionWithChanSize(id string, rxChanSize int) *Session {
	if rxChanSize <= 0 {
		rxChanSize = DefaultRxChanSize
	}
	s := &Session{
		ID:             id,
		txBuf:          make([]byte, 0, 32*1024), // OPT: pre-alloc 32KB to avoid early realloc
		rxQueue:        make(map[uint64]*Envelope),
		lastActivity:   time.Now(),
		RxChan:         make(chan []byte, rxChanSize),
		MaxRxQueueSize: DefaultMaxRxQueueSize,
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
		// OPT-D5: Bound the resequencing queue to prevent OOM from wildly
		// out-of-order packets (e.g., if a mux file arrives days late).
		if s.MaxRxQueueSize > 0 && len(s.rxQueue) >= s.MaxRxQueueSize {
			log.Printf("WARN: session %s rxQueue full (%d), dropping seq %d", s.ID, len(s.rxQueue), env.Seq)
		} else {
			s.rxQueue[env.Seq] = env
		}
	}
	// env.Seq < s.rxSeq → duplicate already delivered, discard silently.

	s.lastActivity = time.Now()
	s.mu.Unlock() // ← RELEASED before any channel operation

	// OPT-D1: Timeout-aware sends prevent goroutine leaks when consumer is dead.
	// Without this, a dead VirtualConn.Read or server relay goroutine causes the
	// dispatch goroutine to hang forever, leaking the download semaphore slot.
	for _, payload := range toDispatch {
		select {
		case s.RxChan <- payload:
			// delivered
		case <-time.After(10 * time.Second):
			log.Printf("WARN: session %s RxChan blocked for 10s, closing session", s.ID)
			// Mark session dead so future ProcessRx calls bail out immediately.
			s.mu.Lock()
			if !s.rxClosed {
				s.rxClosed = true
				s.closed = true
				close(s.RxChan)
			}
			s.mu.Unlock()
			return
		}
	}

	if shouldClose {
		close(s.RxChan)
	}
}
