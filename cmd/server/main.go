package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/NullLatency/flow-driver/internal/config"
	"github.com/NullLatency/flow-driver/internal/httpclient"
	"github.com/NullLatency/flow-driver/internal/storage"
	"github.com/NullLatency/flow-driver/internal/transport"
)

func main() {
	var configPath, gcPath string
	flag.StringVar(&configPath, "c", "config.json", "Path to config file")
	flag.StringVar(&gcPath, "gc", "credentials.json", "Path to Google credentials JSON")
	flag.Parse()

	log.Println("Starting Flow Server...")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appCfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	var backend storage.Backend
	if appCfg.StorageType == "google" {
		customHttpClient := httpclient.NewCustomClient(appCfg.Transport)
		backend = storage.NewGoogleBackend(customHttpClient, gcPath, appCfg.GoogleFolderID)
	} else {
		backend, err = storage.NewLocalBackend(appCfg.LocalDir)
		if err != nil {
			log.Fatalf("Failed to init local storage: %v", err)
		}
	}
	if err := backend.Login(ctx); err != nil {
		log.Fatalf("Backend login failed: %v", err)
	}

	if appCfg.StorageType == "google" && appCfg.GoogleFolderID == "" {
		log.Println("Zero-Config: Searching for Google Drive folder 'Flow-Data'...")
		folderID, err := backend.FindFolder(ctx, "Flow-Data")
		if err != nil {
			log.Fatalf("Failed to search for folder: %v", err)
		}
		if folderID == "" {
			log.Println("Zero-Config: Creating new folder 'Flow-Data'...")
			folderID, err = backend.CreateFolder(ctx, "Flow-Data")
			if err != nil {
				log.Fatalf("Failed to create folder: %v", err)
			}
		} else {
			log.Printf("Zero-Config: Found existing folder %s", folderID)
		}
		appCfg.GoogleFolderID = folderID
		if err := appCfg.Save(configPath); err != nil {
			log.Printf("Warning: Failed to save folder ID: %v", err)
		}
	}

	engine := transport.NewEngine(backend, false, "")
	if appCfg.RefreshRateMs > 0 {
		engine.SetPollRate(appCfg.RefreshRateMs)
	}
	if appCfg.FlushRateMs > 0 {
		engine.SetFlushRate(appCfg.FlushRateMs)
	}
	if appCfg.Compression != nil {
		engine.SetCompression(*appCfg.Compression)
	}
	if appCfg.UploadWorkers > 0 {
		engine.SetUploadWorkers(appCfg.UploadWorkers)
	}
	if appCfg.DownloadWorkers > 0 {
		engine.SetDownloadWorkers(appCfg.DownloadWorkers)
	}

	engine.OnNewSession = func(sessionID, targetAddr string, session *transport.Session) {
		log.Printf("Server: new session %s → %s", sessionID, targetAddr)
		go handleServerConn(ctx, sessionID, targetAddr, session, engine)
	}

	engine.Start(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down...")
	cancel()
}

func handleServerConn(
	parentCtx context.Context,
	sessionID, targetAddr string,
	session *transport.Session,
	engine *transport.Engine,
) {
	// FIX: Per-connection context. When either relay goroutine errors and returns,
	// connCancel() fires. The other goroutine detects ctx.Done() and exits cleanly.
	// The original had no cancellation path — when one goroutine exited via errCh,
	// the other was leaked (blocked on conn.Read or <-session.RxChan) until the OS
	// eventually closed the file descriptor.
	connCtx, connCancel := context.WithCancel(parentCtx)
	defer connCancel()

	// RemoveSession (deferred here) closes session.RxChan, which unblocks the
	// Rx→Conn goroutine below with ok=false instead of hanging forever.
	defer engine.RemoveSession(sessionID)

	conn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		log.Printf("Dial error to %s: %v", targetAddr, err)
		return
	}
	defer conn.Close()

	// FIX: 32KB read buffer (was 4KB).
	// A 100KB HTTP response requires 25 conn.Read calls at 4KB, each invoking
	// EnqueueTx which acquires/releases session.mu. At 32KB: 3-4 calls instead.
	// 8× fewer mutex operations per session per second.
	const readBufSize = 32 * 1024

	errCh := make(chan error, 2)

	// Goroutine 1: Real server → Drive TX queue
	go func() {
		buf := make([]byte, readBufSize)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				session.EnqueueTx(buf[:n])
				// Signal flush loop to upload immediately instead of waiting for ticker.
				engine.SignalFlush()
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("conn.Read error session %s: %v", sessionID, err)
				}
				errCh <- err
				return
			}
		}
	}()

	// Goroutine 2: Drive RX queue → Real server
	go func() {
		for {
			select {
			case <-connCtx.Done():
				errCh <- fmt.Errorf("context cancelled")
				return
			case data, ok := <-session.RxChan:
				if !ok {
					// RxChan was closed by RemoveSession (session ended cleanly).
					errCh <- fmt.Errorf("session closed by remote")
					return
				}
				if len(data) > 0 {
					if _, err := conn.Write(data); err != nil {
						log.Printf("conn.Write error session %s: %v", sessionID, err)
						errCh <- err
						return
					}
				}
			}
		}
	}()

	// Wait for either goroutine to finish. connCancel() (deferred above) then
	// signals the other goroutine via connCtx.Done().
	<-errCh
}
