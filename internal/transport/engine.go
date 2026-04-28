package transport

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NullLatency/flow-driver/internal/storage"
)

// Engine manages local sessions, periodically flushes Tx buffers to Drive files,
// and polls for incoming Rx files from the peer.
type Engine struct {
	backend storage.Backend
	myDir   Direction
	peerDir Direction
	id      string // ClientID (client only)

	sessions  map[string]*Session
	sessionMu sync.RWMutex

	closedSessions   map[string]time.Time
	closedSessionsMu sync.Mutex

	pollTicker  time.Duration
	flushTicker time.Duration
	idleTimeout time.Duration

	OnNewSession func(sessionID, targetAddr string, s *Session)

	// OPT: Separate semaphores so uploads never starve downloads.
	// Downloads are on the critical response path; uploads are fire-and-forget.
	uploadSem   chan struct{}
	downloadSem chan struct{}

	// OPT-02/flushSignal: wakes the flush loop immediately on new data instead
	// of waiting up to flushTicker (300ms). This is the single largest TX latency
	// reduction — data goes to Drive as fast as one HTTP call, not after a timer.
	flushSignal chan struct{}

	// OPT-13: processed map with TTL timestamps instead of bool.
	// Entries are removed after 5 minutes, which guarantees in-flight files are
	// never evicted while they're being processed. The bulk-reset (`make(map)`)
	// from the original caused a window where the same file could be re-downloaded
	// (wasting API quota), though the duplicate data is harmless (old Seq is
	// discarded by ProcessRx).
	processed   map[string]time.Time
	processedMu sync.Mutex

	// OPT-05: Track our own uploads in memory so cleanupLoop doesn't need to
	// call ListQuery (an API call) every 5 seconds just to find old files.
	uploadLog   []uploadRecord
	uploadLogMu sync.Mutex

	// OPT-16: Atomic counter ensures unique filenames even when two flushes
	// happen within the same nanosecond (possible on Windows where
	// time.Now().UnixNano() has 100ns resolution).
	uploadSeq uint64

	// OPT: gzip compress mux file payloads before upload.
	// Auto-detected on receive (gzip magic 0x1f,0x8b vs envelope magic 0x1f,0x20+).
	compression bool

	// OPT-E4: Pre-computed base64url-encoded client ID.
	// Avoids calling base64.RawURLEncoding.EncodeToString every poll + every flush.
	encodedID string
}

type uploadRecord struct {
	filename   string
	uploadedAt time.Time
}

// bufPool reuses bytes.Buffer to reduce GC pressure in flushAll.
// OPT-H5: Buffers larger than 256KB are discarded instead of returned to the pool
// to prevent a single bulk transfer from permanently inflating pool memory.
const maxPoolBufSize = 256 * 1024

var bufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

func putBuf(buf *bytes.Buffer) {
	if buf.Cap() > maxPoolBufSize {
		return // let GC collect oversized buffers
	}
	bufPool.Put(buf)
}

// OPT-C1: Pool gzip readers to avoid ~40KB alloc per download.
var gzipReaderPool = sync.Pool{}

// OPT-C2: Pool bufio.Reader to avoid 64KB alloc per download.
var bufioReaderPool = sync.Pool{
	New: func() any {
		return bufio.NewReaderSize(nil, 64*1024)
	},
}

func NewEngine(backend storage.Backend, isClient bool, clientID string) *Engine {
	// OPT-E4: Pre-compute the base64url-encoded client ID once at startup.
	var encoded string
	if clientID != "" {
		encoded = base64.RawURLEncoding.EncodeToString([]byte(clientID))
	}

	e := &Engine{
		backend:        backend,
		id:             clientID,
		encodedID:      encoded,
		sessions:       make(map[string]*Session),
		closedSessions: make(map[string]time.Time),
		processed:      make(map[string]time.Time),
		// OPT: Lowered from 500ms. Faster base poll = lower worst-case latency.
		pollTicker: 200 * time.Millisecond,
		// OPT: Lowered from 300ms. With flushSignal this is just the fallback.
		flushTicker: 200 * time.Millisecond,
		idleTimeout: 60 * time.Second, // OPT-09: was 10s, too short for HTTP keep-alive
		// OPT: Increased from 4/8 to 16/16. More concurrent API calls = higher throughput.
		// The HTTP/2 connection handles multiplexing; semaphores prevent quota exhaustion.
		uploadSem:   make(chan struct{}, 16),
		downloadSem: make(chan struct{}, 16),
		flushSignal: make(chan struct{}, 1),
		compression: true, // OPT: enabled by default
	}
	if isClient {
		e.myDir = DirReq
		e.peerDir = DirRes
	} else {
		e.myDir = DirRes
		e.peerDir = DirReq
	}
	return e
}

func (e *Engine) SetRefreshRate(ms int) {
	if ms > 0 {
		e.pollTicker = time.Duration(ms) * time.Millisecond
		if e.flushTicker == 200*time.Millisecond {
			e.flushTicker = time.Duration(ms) * time.Millisecond
		}
	}
}

func (e *Engine) SetPollRate(ms int) {
	if ms > 0 {
		e.pollTicker = time.Duration(ms) * time.Millisecond
	}
}

func (e *Engine) SetFlushRate(ms int) {
	if ms > 0 {
		e.flushTicker = time.Duration(ms) * time.Millisecond
	}
}

// SetCompression enables or disables gzip compression on uploads.
// Receive-side auto-detects, so mixed versions interoperate safely.
func (e *Engine) SetCompression(enabled bool) {
	e.compression = enabled
}

// SetUploadWorkers sets the max concurrent upload goroutines.
func (e *Engine) SetUploadWorkers(n int) {
	if n > 0 {
		e.uploadSem = make(chan struct{}, n)
	}
}

// SetDownloadWorkers sets the max concurrent download goroutines.
func (e *Engine) SetDownloadWorkers(n int) {
	if n > 0 {
		e.downloadSem = make(chan struct{}, n)
	}
}

// SignalFlush wakes the flush loop immediately. Call after any EnqueueTx.
// Safe from any goroutine, never blocks.
func (e *Engine) SignalFlush() {
	select {
	case e.flushSignal <- struct{}{}:
	default:
	}
}

func (e *Engine) Start(ctx context.Context) {
	go e.flushLoop(ctx)
	go e.pollLoop(ctx)
	go e.cleanupLoop(ctx)
}

func (e *Engine) GetSession(id string) *Session {
	e.sessionMu.RLock()
	defer e.sessionMu.RUnlock()
	return e.sessions[id]
}

func (e *Engine) AddSession(s *Session) {
	e.sessionMu.Lock()
	defer e.sessionMu.Unlock()
	e.sessions[s.ID] = s
	log.Printf("Engine.AddSession: %s (total: %d)", s.ID, len(e.sessions))
}

func (e *Engine) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(e.flushTicker)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.flushAll(ctx)
		case <-e.flushSignal:
			e.flushAll(ctx)
			// Drain ticker so we don't do a redundant flush immediately after.
			select {
			case <-ticker.C:
			default:
			}
		}
	}
}

func (e *Engine) flushAll(ctx context.Context) {
	// OPT-03 FIX: was e.sessionMu.Lock() (write lock).
	// flushAll only reads the sessions map to build a snapshot slice.
	// A write lock blocked all concurrent RLock callers (GetSession, pollLoop's
	// session lookup, AddSession's callers) for the entire snapshot duration —
	// every 300ms. One word change; immediate effect.
	e.sessionMu.RLock()
	sessions := make([]*Session, 0, len(e.sessions))
	for _, s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.sessionMu.RUnlock()

	// OPT: Pre-allocate with session count hint.
	muxes := make(map[string][]Envelope, len(sessions))
	var closedSessionIDs []string

	for _, s := range sessions {
		s.mu.Lock()

		if time.Since(s.lastActivity) > e.idleTimeout {
			s.closed = true
		}

		shouldSend := len(s.txBuf) > 0 || (s.txSeq == 0 && e.myDir == DirReq) || s.closed

		if !shouldSend {
			s.mu.Unlock()
			continue
		}

		payload := s.txBuf
		s.txBuf = nil
		s.txCond.Broadcast()

		env := Envelope{
			SessionID:  s.ID,
			Seq:        s.txSeq,
			Payload:    payload,
			Close:      s.closed,
			TargetAddr: s.TargetAddr,
		}

		s.txSeq++
		if s.closed {
			closedSessionIDs = append(closedSessionIDs, s.ID)
		}

		cid := s.ClientID
		if cid == "" && e.myDir == DirReq {
			cid = e.id
		}

		muxes[cid] = append(muxes[cid], env)
		s.mu.Unlock()
	}

	for cid, mux := range muxes {
		fnameCID := cid
		if fnameCID == "" {
			fnameCID = "unknown"
		}

		// OPT-E4: Use cached encoded CID when it matches, avoiding per-flush base64 encoding.
		encodedCID := e.encodedID
		if fnameCID != e.id {
			encodedCID = base64.RawURLEncoding.EncodeToString([]byte(fnameCID))
		}
		seq := atomic.AddUint64(&e.uploadSeq, 1)
		filename := fmt.Sprintf("%s-%s-mux-%d-%d.bin", e.myDir, encodedCID, time.Now().UnixNano(), seq)

		// OPT-10 FIX: Pre-serialize to a buffer BEFORE the goroutine.
		buf := bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		encodeOk := true
		for _, env := range mux {
			if err := env.Encode(buf); err != nil {
				log.Printf("encode error for %s: %v", filename, err)
				encodeOk = false
				break
			}
		}
		if !encodeOk {
			bufPool.Put(buf)
			continue
		}

		// OPT: Compress the serialized envelope data with gzip.
		// Receiver auto-detects gzip by magic bytes (0x1f,0x8b).
		var serialized []byte
		if e.compression && buf.Len() > 0 {
			compBuf := bufPool.Get().(*bytes.Buffer)
			compBuf.Reset()
			// Issue 5 fix: handle gzip.NewWriterLevel error instead of discarding.
			gw, gwErr := gzip.NewWriterLevel(compBuf, gzip.BestSpeed)
			if gwErr != nil {
				log.Printf("gzip writer error, sending uncompressed: %v", gwErr)
				serialized = make([]byte, buf.Len())
				copy(serialized, buf.Bytes())
				putBuf(buf)
				putBuf(compBuf)
			} else {
				gw.Write(buf.Bytes())
				gw.Close()
				// Issue 6 fix: only use compressed output if actually smaller.
				// Most HTTPS payloads are already compressed; gzip makes them bigger.
				if compBuf.Len() < buf.Len() {
					putBuf(buf)
					serialized = make([]byte, compBuf.Len())
					copy(serialized, compBuf.Bytes())
				} else {
					serialized = make([]byte, buf.Len())
					copy(serialized, buf.Bytes())
					putBuf(buf)
				}
				putBuf(compBuf)
			}
		} else {
			serialized = make([]byte, buf.Len())
			copy(serialized, buf.Bytes())
			putBuf(buf)
		}

		go func(fname string, data []byte) {
			// Bug 2 fix: respect ctx.Done() when acquiring semaphore.
			// Without this, goroutines block forever on shutdown if all slots are occupied.
			select {
			case e.uploadSem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-e.uploadSem }()

			const maxRetries = 3
			for attempt := 0; attempt < maxRetries; attempt++ {
				if attempt > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
					}
					log.Printf("upload retry %d/%d: %s", attempt+1, maxRetries, fname)
				}

				// OPT-B5: Scale upload timeout with payload size.
				// Base 10s + 1s per 50KB. A 500KB mux gets 20s; a 5MB mux gets 110s.
				timeout := 10*time.Second + time.Duration(len(data)/50000)*time.Second
				if timeout < 10*time.Second {
					timeout = 10 * time.Second
				}
				uploadCtx, cancel := context.WithTimeout(ctx, timeout)
				err := e.backend.Upload(uploadCtx, fname, bytes.NewReader(data))
				cancel()

				if err == nil {
					// OPT-05: Record successful upload in memory so cleanupLoop
					// can delete it without calling ListQuery.
					e.uploadLogMu.Lock()
					e.uploadLog = append(e.uploadLog, uploadRecord{fname, time.Now()})
					e.uploadLogMu.Unlock()
					return
				}
				if ctx.Err() != nil {
					return
				}
				log.Printf("upload error %s (attempt %d/%d): %v", fname, attempt+1, maxRetries, err)
			}
			log.Printf("WARN: permanent data loss for %s after %d retries", fname, maxRetries)
		}(filename, serialized)
	}

	for _, id := range closedSessionIDs {
		e.RemoveSession(id)
	}
}

func (e *Engine) pollLoop(ctx context.Context) {
	currentPollInterval := e.pollTicker
	// OPT: Reduced from 5s to 2s — faster cold-start when a new client connects
	// after the server has backed off from idle.
	maxPollInterval := 2 * time.Second
	timer := time.NewTimer(currentPollInterval)
	defer timer.Stop()

	// OPT: Persistent timer for inner-loop sleep. Avoids creating a new timer
	// per iteration via time.After() which leaks until it fires.
	sleepTimer := time.NewTimer(100 * time.Millisecond)
	defer sleepTimer.Stop()
	if !sleepTimer.Stop() {
		<-sleepTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			// OPT-02 FIX: Replaced `goto pollAgain` with a proper inner loop.
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				// Client-side optimization: if no active sessions exist, responses
				// are mathematically impossible — skip the poll entirely.
				if e.myDir == DirReq {
					e.sessionMu.RLock()
					count := len(e.sessions)
					e.sessionMu.RUnlock()
					if count == 0 {
						break
					}
				}

				prefix := string(e.peerDir) + "-"
				if e.myDir == DirReq {
					// OPT-E4: Use cached encoded CID instead of re-encoding each poll.
					prefix += e.encodedID + "-mux-"
				}
				// Server polls for all client request files (prefix = "req-")

				listCtx, listCancel := context.WithTimeout(ctx, 10*time.Second)
				files, err := e.backend.ListQuery(listCtx, prefix)
				listCancel()
				if err != nil {
					log.Printf("poll list error: %v", err)
					// OPT-G2: Back off on list failure to avoid hammering a failing API.
					currentPollInterval = currentPollInterval * 2
					if currentPollInterval > maxPollInterval {
						currentPollInterval = maxPollInterval
					}
					break
				}

				if len(files) == 0 {
					if e.myDir == DirRes {
						e.sessionMu.RLock()
						activeSessions := len(e.sessions)
						e.sessionMu.RUnlock()

						if activeSessions == 0 {
							currentPollInterval += 250 * time.Millisecond
							if currentPollInterval > maxPollInterval {
								currentPollInterval = maxPollInterval
							}
						} else {
							currentPollInterval = e.pollTicker
						}
					}
					break
				}

				currentPollInterval = e.pollTicker

				// OPT: Sort files by timestamp so oldest (most latency-critical) are
				// processed first. Files are named "{dir}-{cid}-mux-{ts}-{seq}.bin".
				// OPT-A3: Skip sort for 0-1 files — no-op but avoids function call overhead.
				if len(files) > 1 {
					sort.Strings(files)
				}

				for _, f := range files {
					// Parse timestamp from new filename format:
					// "{dir}-{base64cid}-mux-{ts}-{seq}.bin"
					muxParts := strings.SplitN(f, "-mux-", 2)
					if len(muxParts) == 2 {
						tsAndSeq := strings.TrimSuffix(muxParts[1], ".bin")
						tsParts := strings.SplitN(tsAndSeq, "-", 2)
						ts, _ := strconv.ParseInt(tsParts[0], 10, 64)
						if ts > 0 && time.Since(time.Unix(0, ts)) > 5*time.Minute {
							e.backend.Delete(ctx, f)
							continue
						}
					}

					e.processedMu.Lock()
					_, already := e.processed[f]
					if !already {
						e.processed[f] = time.Now()
					}
					e.processedMu.Unlock()

					if already {
						continue
					}

					go e.downloadAndProcess(ctx, f)
				}

				// OPT: Use persistent timer instead of time.After to avoid timer leak.
				sleepTimer.Reset(100 * time.Millisecond)
				select {
				case <-ctx.Done():
					return
				case <-sleepTimer.C:
				}
			}

			timer.Reset(currentPollInterval)
		}
	}
}

// downloadAndProcess handles downloading, decompressing, and dispatching a single mux file.
// OPT: Extracted from pollLoop for clarity and to add download retry logic.
func (e *Engine) downloadAndProcess(ctx context.Context, fname string) {
	// Bug 2 fix: respect ctx.Done() when acquiring semaphore.
	select {
	case e.downloadSem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-e.downloadSem }()

	// OPT: Download with retry (1 retry on failure).
	var rc io.ReadCloser
	var dlCancel context.CancelFunc
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if dlCancel != nil {
				dlCancel()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		var dlCtx context.Context
		dlCtx, dlCancel = context.WithTimeout(ctx, 15*time.Second)
		rc, err = e.backend.Download(dlCtx, fname)
		if err == nil {
			break
		}
	}
	if err != nil {
		if dlCancel != nil {
			dlCancel()
		}
		log.Printf("download error %s: %v", fname, err)
		e.processedMu.Lock()
		delete(e.processed, fname)
		e.processedMu.Unlock()
		return
	}
	// Cancel the download timeout AFTER we're done reading the body.
	defer dlCancel()
	defer rc.Close()

	// OPT: Auto-detect gzip compression.
	// Gzip files start with 0x1f,0x8b. Raw envelopes start with 0x1f,{sidLen}
	// where sidLen is typically 0x20 (32 hex chars). No ambiguity.
	var reader io.Reader
	peek := make([]byte, 2)
	if _, err := io.ReadFull(rc, peek); err != nil {
		log.Printf("peek error %s: %v", fname, err)
		e.cleanupProcessed(fname)
		return
	}
	if peek[0] == 0x1f && peek[1] == 0x8b {
		// Gzip compressed — decompress transparently.
		// OPT-C1: Reuse pooled gzip reader to avoid ~40KB alloc per download.
		multiR := io.MultiReader(bytes.NewReader(peek), rc)
		if pooled := gzipReaderPool.Get(); pooled != nil {
			gr := pooled.(*gzip.Reader)
			if err := gr.Reset(multiR); err != nil {
				log.Printf("gzip reset error %s: %v", fname, err)
				e.cleanupProcessed(fname)
				return
			}
			defer func() { gr.Close(); gzipReaderPool.Put(gr) }()
			reader = gr
		} else {
			gr, err := gzip.NewReader(multiR)
			if err != nil {
				log.Printf("gzip init error %s: %v", fname, err)
				e.cleanupProcessed(fname)
				return
			}
			defer func() { gr.Close(); gzipReaderPool.Put(gr) }()
			reader = gr
		}
	} else {
		// Raw envelope data — prepend the peeked bytes.
		reader = io.MultiReader(bytes.NewReader(peek), rc)
	}

	// OPT-C2: Reuse pooled bufio.Reader to avoid 64KB alloc per download.
	br := bufioReaderPool.Get().(*bufio.Reader)
	br.Reset(reader)
	defer bufioReaderPool.Put(br)

	// Extract clientID from base64-encoded filename part.
	// Format: "{dir}-{base64cid}-mux-{ts}-{seq}.bin"
	var fileClientID string
	muxP := strings.SplitN(fname, "-mux-", 2)
	if len(muxP) == 2 {
		dirAndCID := strings.SplitN(muxP[0], "-", 2)
		if len(dirAndCID) == 2 {
			cidBytes, err := base64.RawURLEncoding.DecodeString(dirAndCID[1])
			if err == nil {
				fileClientID = string(cidBytes)
			}
		}
	}

	// Phase 1: Decode ALL envelopes into per-session buckets.
	// OPT-C4: Abort after 5 consecutive decode errors to stop processing corrupt files.
	sessionEnvs := make(map[string][]*Envelope)
	consecutiveErrors := 0
	const maxDecodeErrors = 5
	for {
		env := &Envelope{}
		if err := env.Decode(br); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				consecutiveErrors++
				log.Printf("mux decode error %s: %v", fname, err)
				if consecutiveErrors >= maxDecodeErrors {
					log.Printf("WARN: aborting decode of %s after %d consecutive errors", fname, maxDecodeErrors)
					break
				}
				continue // try next envelope
			}
			break
		}
		consecutiveErrors = 0 // reset on success

		e.closedSessionsMu.Lock()
		_, isClosed := e.closedSessions[env.SessionID]
		e.closedSessionsMu.Unlock()
		if isClosed {
			continue
		}

		sessionEnvs[env.SessionID] = append(sessionEnvs[env.SessionID], env)
	}

	// Phase 2: For each session, ensure it exists, then dispatch
	// a goroutine to deliver its envelopes independently.
	var dispatchWg sync.WaitGroup
	for sid, envs := range sessionEnvs {
		firstEnv := envs[0]

		e.sessionMu.Lock()
		s, exists := e.sessions[sid]
		if !exists && e.myDir == DirRes && e.OnNewSession != nil {
			s = NewSession(sid)
			s.ClientID = fileClientID
			e.sessions[sid] = s
			e.sessionMu.Unlock()
			log.Printf("Engine: new session %s for client %s", sid, fileClientID)
			e.OnNewSession(sid, firstEnv.TargetAddr, s)
		} else {
			e.sessionMu.Unlock()
		}

		if s == nil {
			continue
		}

		dispatchWg.Add(1)
		go func(sess *Session, batch []*Envelope) {
			defer dispatchWg.Done()
			for _, env := range batch {
				sess.ProcessRx(env)
			}
		}(s, envs)
	}
	dispatchWg.Wait()

	delCtx, delCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := e.backend.Delete(delCtx, fname); err != nil {
		log.Printf("delete error %s: %v", fname, err)
	} else {
		e.processedMu.Lock()
		delete(e.processed, fname)
		e.processedMu.Unlock()
	}
	delCancel()
}

// cleanupProcessed removes a file from the processed map on error.
func (e *Engine) cleanupProcessed(fname string) {
	e.processedMu.Lock()
	delete(e.processed, fname)
	e.processedMu.Unlock()
}

// RemoveSession removes a session from the active map and closes its RxChan.
//
// OPT-12 FIX: The original never closed RxChan. Any goroutine blocked on
// <-session.RxChan (server's Rx→Conn relay goroutine, or VirtualConn.Read
// on the client) would hang indefinitely, leaking the goroutine.
//
// At 100 sessions/hour × 24 hours × 8KB stack = ~19MB/day of leaked memory.
// On a 512MB VPS running for a week: ~134MB, enough to cause OOM.
//
// The rxClosed flag (set in ProcessRx under lock when a Close envelope arrives,
// and here under lock) prevents a double-close panic if both paths race.
func (e *Engine) RemoveSession(id string) {
	e.sessionMu.Lock()
	s := e.sessions[id]
	delete(e.sessions, id)
	e.sessionMu.Unlock()

	if s != nil {
		s.mu.Lock()
		if !s.rxClosed {
			s.rxClosed = true
			s.closed = true
			close(s.RxChan) // Unblocks all receivers: ok=false → io.EOF
		}
		s.txCond.Broadcast()
		s.mu.Unlock()
	}

	e.closedSessionsMu.Lock()
	e.closedSessions[id] = time.Now()
	e.closedSessionsMu.Unlock()
}

func (e *Engine) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Issue 7 fix: Increased from 15s to 60s to match idleTimeout.
			// On Iran's intermittent connections, delayed files >15s would
			// trigger phantom sessions on the server.
			e.closedSessionsMu.Lock()
			for id, t := range e.closedSessions {
				if time.Since(t) > 60*time.Second {
					delete(e.closedSessions, id)
				}
			}
			e.closedSessionsMu.Unlock()

			// OPT-13: TTL-based eviction for processed map.
			e.processedMu.Lock()
			cutoff := time.Now().Add(-5 * time.Minute)
			for k, t := range e.processed {
				if t.Before(cutoff) {
					delete(e.processed, k)
				}
			}
			if len(e.processed) > 5000 {
				log.Printf("WARN: processed map has %d entries — Drive deletes consistently failing", len(e.processed))
			}
			e.processedMu.Unlock()

			// OPT-05: Delete our own old uploads using the in-memory log.
			e.uploadLogMu.Lock()
			remaining := e.uploadLog[:0]
			var toDelete []string
			for _, r := range e.uploadLog {
				if time.Since(r.uploadedAt) > 60*time.Second {
					toDelete = append(toDelete, r.filename)
				} else {
					remaining = append(remaining, r)
				}
			}
			e.uploadLog = remaining
			e.uploadLogMu.Unlock()

			for _, fname := range toDelete {
				go func(f string) {
					delCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					e.backend.Delete(delCtx, f)
					cancel()
				}(fname)
			}
		}
	}
}
