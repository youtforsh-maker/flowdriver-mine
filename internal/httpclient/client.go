package httpclient

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// TransportConfig defines the rules for bypassing censorship
// to reach the underlying APIs.
type TransportConfig struct {
	// TargetIP is the IP address to dial instead of the DNS resolved IP.
	// E.g., "216.239.38.120:443"
	TargetIP string

	// SNI is the Server Name Indication to use during the TLS handshake.
	// E.g., "google.com"
	SNI string

	// HostHeader is the Host header to inject in HTTP requests.
	// E.g., "www.googleapis.com"
	HostHeader string

	// InsecureSkipVerify allows bypassing certificate validation if necessary.
	InsecureSkipVerify bool
}

// hostRewriteTransport is an http.RoundTripper that rewrites the Host header.
type hostRewriteTransport struct {
	Transport  http.RoundTripper
	HostHeader string
}

func (t *hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.HostHeader != "" {
		req.Host = t.HostHeader
	}
	return t.Transport.RoundTrip(req)
}

// NewCustomClient creates an http.Client configured to bypass DNS
// and manipulate TLS/HTTP headers as specified in the config.
func NewCustomClient(cfg TransportConfig) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		// Force dialing the TargetIP instead of resolving the request URL's host.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if cfg.TargetIP != "" {
				return dialer.DialContext(ctx, "tcp", cfg.TargetIP)
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{
			ServerName:         cfg.SNI,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		},
		ForceAttemptHTTP2: true,
		MaxIdleConns:      100,
		// OPT: Was omitted (Go default=2). All calls hit googleapis.com,
		// so only 2 connections were reused — the rest required full TCP+TLS
		// handshake every time. Set to 20 for proper HTTP/2 multiplexing.
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		// OPT: Was 1s. Waits for "100 Continue" before sending body.
		// Google responds in <5ms, but if a middlebox drops the 100 response,
		// the client stalls for the full timeout. Set to 0 to send immediately.
		ExpectContinueTimeout: 0,
		// OPT: Larger buffers reduce the number of read/write syscalls
		// during multipart upload and file download.
		WriteBufferSize: 32 << 10, // 32KB
		ReadBufferSize:  32 << 10, // 32KB
	}

	var rt http.RoundTripper = transport
	if cfg.HostHeader != "" {
		rt = &hostRewriteTransport{
			Transport:  transport,
			HostHeader: cfg.HostHeader,
		}
	}

	return &http.Client{
		Transport: rt,
		Timeout:   60 * time.Second,
	}
}

// WarmUp primes the HTTP connection pool by making a lightweight request.
// OPT-F7: First real request pays ~300-500ms for TCP+TLS handshake.
// Calling this at startup moves that cost out of the critical path.
func WarmUp(client *http.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://www.googleapis.com/drive/v3/about?fields=user", nil)
	if err != nil {
		return
	}
	// No auth header — will get 401, but TCP+TLS is established and pooled.
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
