package storage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// QuotaError indicates a Google API quota or permission error (429/403).
// OPT-G6: Callers can type-assert to back off appropriately.
type QuotaError struct {
	StatusCode int
	Body       string
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("quota/permission error %d: %s", e.StatusCode, e.Body)
}

type oauthClientJSON struct {
	Installed struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		AuthURI      string   `json:"auth_uri"`
		TokenURI     string   `json:"token_uri"`
		RedirectURIs []string `json:"redirect_uris"`
	} `json:"installed"`
}

type tokenCache struct {
	RefreshToken string `json:"refresh_token"`
}

// GoogleBackend implements Backend using raw Google Drive REST APIs.
type GoogleBackend struct {
	httpClient *http.Client
	saPath     string
	folderID   string

	clientID     string
	clientSecret string
	tokenURI     string
	redirectURI  string

	token        string
	refreshToken string
	tokenEx      time.Time
	// OPT-17 FIX: Changed from sync.Mutex to sync.RWMutex with double-checked locking.
	mu sync.RWMutex

	fileIDs   map[string]string
	fileIdsMu sync.RWMutex

	// OPT-G10: Track insertion order for LRU eviction.
	fileIDOrder   []string

	// OPT-F9: Circuit breaker — pause API calls after consecutive failures.
	consecFails  int64 // atomic
	lastFailTime int64 // atomic, UnixNano
}

func NewGoogleBackend(client *http.Client, saPath, folderID string) *GoogleBackend {
	return &GoogleBackend{
		httpClient: client,
		saPath:     saPath,
		folderID:   folderID,
		fileIDs:    make(map[string]string),
		fileIDOrder: make([]string, 0, 256),
	}
}

// OPT-F9: checkCircuitBreaker returns an error if too many consecutive failures occurred.
// After 5 failures, all API calls pause for 5 seconds to prevent retry storms.
const circuitBreakerThreshold = 5
const circuitBreakerCooldown = 5 * time.Second

func (b *GoogleBackend) checkCircuitBreaker() error {
	fails := atomic.LoadInt64(&b.consecFails)
	if fails >= circuitBreakerThreshold {
		lastFail := time.Unix(0, atomic.LoadInt64(&b.lastFailTime))
		if time.Since(lastFail) < circuitBreakerCooldown {
			return fmt.Errorf("circuit breaker open: %d consecutive failures, cooling down", fails)
		}
		// Cooldown expired, reset and allow retry
		atomic.StoreInt64(&b.consecFails, 0)
	}
	return nil
}

func (b *GoogleBackend) recordSuccess() {
	atomic.StoreInt64(&b.consecFails, 0)
}

func (b *GoogleBackend) recordFailure() {
	atomic.AddInt64(&b.consecFails, 1)
	atomic.StoreInt64(&b.lastFailTime, time.Now().UnixNano())
}

func (b *GoogleBackend) Login(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	data, err := os.ReadFile(b.saPath)
	if err != nil {
		return fmt.Errorf("failed to read Client Secret JSON %s: %w", b.saPath, err)
	}
	var oauthJSON oauthClientJSON
	if err := json.Unmarshal(data, &oauthJSON); err != nil {
		return fmt.Errorf("failed to parse Client Secret JSON: %w", err)
	}

	b.clientID = oauthJSON.Installed.ClientID
	b.clientSecret = oauthJSON.Installed.ClientSecret
	b.tokenURI = "https://www.googleapis.com/oauth2/v4/token"
	if len(oauthJSON.Installed.RedirectURIs) > 0 {
		b.redirectURI = oauthJSON.Installed.RedirectURIs[0]
	} else {
		b.redirectURI = "http://localhost"
	}

	tokenCachePath := b.saPath + ".token"

	if cacheData, err := os.ReadFile(tokenCachePath); err == nil {
		var cache tokenCache
		if err := json.Unmarshal(cacheData, &cache); err == nil && cache.RefreshToken != "" {
			b.refreshToken = cache.RefreshToken
			return b.refreshAccessToken(ctx)
		}
	}

	authURI := oauthJSON.Installed.AuthURI
	link := fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&response_type=code&scope=https://www.googleapis.com/auth/drive.file&access_type=offline",
		authURI, url.QueryEscape(b.clientID), url.QueryEscape(b.redirectURI))

	fmt.Printf("\n==================== OAUTH AUTHENTICATION REQUIRED ====================\n")
	fmt.Printf("1. Open this URL in your browser:\n\n%s\n\n", link)
	fmt.Printf("2. Authenticate and accept the permissions.\n")
	fmt.Printf("3. Copy the FULL redirected URL from your browser and paste it below:\n\n")
	fmt.Printf("Enter URL or Code: ")

	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	code := input
	if strings.HasPrefix(input, "http") {
		if u, err := url.Parse(input); err == nil {
			if qCode := u.Query().Get("code"); qCode != "" {
				code = qCode
			}
		}
	}
	if code == "" {
		return fmt.Errorf("invalid authorization code")
	}

	if err := b.exchangeCode(ctx, code); err != nil {
		return err
	}

	cache := tokenCache{RefreshToken: b.refreshToken}
	cacheBytes, _ := json.MarshalIndent(cache, "", "  ")
	if err := os.WriteFile(tokenCachePath, cacheBytes, 0600); err != nil {
		fmt.Printf("WARNING: Failed to save refresh token: %v\n", err)
	} else {
		fmt.Printf("Saved refresh token. Future startups will be silent.\n")
	}
	fmt.Printf("OAuth Authentication Successful!\n=======================================================================\n\n")
	return nil
}

func (b *GoogleBackend) exchangeCode(ctx context.Context, code string) error {
	v := url.Values{}
	v.Set("grant_type", "authorization_code")
	v.Set("code", code)
	v.Set("client_id", b.clientID)
	v.Set("client_secret", b.clientSecret)
	v.Set("redirect_uri", b.redirectURI)
	return b.executeTokenRequest(ctx, v)
}

func (b *GoogleBackend) refreshAccessToken(ctx context.Context) error {
	v := url.Values{}
	v.Set("grant_type", "refresh_token")
	v.Set("refresh_token", b.refreshToken)
	v.Set("client_id", b.clientID)
	v.Set("client_secret", b.clientSecret)
	return b.executeTokenRequest(ctx, v)
}

func (b *GoogleBackend) executeTokenRequest(ctx context.Context, v url.Values) error {
	req, err := http.NewRequestWithContext(ctx, "POST", b.tokenURI, strings.NewReader(v.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token request returned %d: %s", resp.StatusCode, body)
	}

	var resData struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resData); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}

	b.token = resData.AccessToken
	if resData.RefreshToken != "" {
		b.refreshToken = resData.RefreshToken
	}
	b.tokenEx = time.Now().Add(time.Duration(resData.ExpiresIn-60) * time.Second)
	return nil
}

// getValidToken returns a valid OAuth2 access token.
// Uses RWMutex with double-checked locking (see field comment for full explanation).
func (b *GoogleBackend) getValidToken(ctx context.Context) (string, error) {
	// Fast path (common case): token is valid, all goroutines read concurrently.
	b.mu.RLock()
	if time.Now().Before(b.tokenEx) {
		tok := b.token
		b.mu.RUnlock()
		return tok, nil
	}
	b.mu.RUnlock()

	// Slow path: token expired. Acquire write lock to refresh.
	b.mu.Lock()
	defer b.mu.Unlock()
	// Double-check: another goroutine may have refreshed between our RUnlock and Lock.
	if time.Now().Before(b.tokenEx) {
		return b.token, nil
	}
	if err := b.refreshAccessToken(ctx); err != nil {
		return "", err
	}
	return b.token, nil
}

func (b *GoogleBackend) Upload(ctx context.Context, filename string, data io.Reader) error {
	if err := b.checkCircuitBreaker(); err != nil {
		return err
	}
	tok, err := b.getValidToken(ctx)
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	metaWriter := multipart.NewWriter(pw)

	// OPT-08 FIX: Propagate errors from the upload goroutine via pw.CloseWithError.
	//
	// The original goroutine used `_` for all CreatePart and io.Copy errors.
	// If io.Copy failed mid-transfer (network blip, slow pipe), pw.Close() was
	// called normally and the HTTP client received a truncated body. Google Drive
	// returned a 400/500, the engine logged "upload error", and the data —
	// already cleared from txBuf by the time the goroutine ran — was permanently lost.
	//
	// With pw.CloseWithError(err), the http.Client.Do() call returns that error
	// immediately, giving the caller (flushAll's retry loop) a clean signal to retry.
	go func() {
		var encErr error
		defer func() {
			metaWriter.Close()
			if encErr != nil {
				pw.CloseWithError(encErr)
			} else {
				pw.Close()
			}
		}()

		h := make(textproto.MIMEHeader)
		h.Set("Content-Type", "application/json; charset=UTF-8")
		part1, err := metaWriter.CreatePart(h)
		if err != nil {
			encErr = err
			return
		}
		meta := map[string]interface{}{"name": filename}
		if b.folderID != "" {
			meta["parents"] = []string{b.folderID}
		}
		if err := json.NewEncoder(part1).Encode(meta); err != nil {
			encErr = err
			return
		}

		h = make(textproto.MIMEHeader)
		h.Set("Content-Type", "application/octet-stream")
		part2, err := metaWriter.CreatePart(h)
		if err != nil {
			encErr = err
			return
		}
		if _, err := io.Copy(part2, data); err != nil {
			encErr = err
		}
	}()

	// OPT: Added fields=id so Google returns only the file ID, not full metadata.
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart&fields=id", pr)
	if err != nil {
		pr.CloseWithError(err)
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", metaWriter.FormDataContentType())

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.recordFailure()
		return err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		b.recordFailure()
		// OPT-G6: Return typed error for quota/permission failures.
		if resp.StatusCode == 429 || resp.StatusCode == 403 {
			return &QuotaError{StatusCode: resp.StatusCode, Body: string(respBytes)}
		}
		return fmt.Errorf("upload returned %d: %s", resp.StatusCode, respBytes)
	}
	b.recordSuccess()

	// Store the file ID returned by Drive so Download and Delete can find it
	// immediately without waiting for a ListQuery to run first.
	var uploadResult struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(bytes.NewReader(respBytes)).Decode(&uploadResult); err == nil && uploadResult.ID != "" {
		b.fileIdsMu.Lock()
		b.fileIDs[filename] = uploadResult.ID
		b.fileIDOrder = append(b.fileIDOrder, filename)
		b.fileIdsMu.Unlock()
	}

	return nil
}

// ListQuery lists all files whose name starts with prefix.
//
// OPT-11 FIX: Added pagination via nextPageToken.
//
// The original made a single Drive API call with no pageSize parameter.
// Drive's default pageSize is 100. At 10 active clients × 300ms flush rate:
// ~33 new files/second appear in the folder. Over a 10-second cleanup window:
// 330 files. The single-page query silently returned only 100 of them. The
// other 230 files weren't downloaded in that poll cycle; those sessions waited
// an extra 100–500ms. At 50 clients the folder can hold 1,650+ files — four
// pages needed per poll.
//
// We request pageSize=1000 (Drive's maximum) and follow nextPageToken until
// exhausted. For up to 1,000 files this is still one API call. Beyond 1,000
// (requiring 100+ concurrent very active clients), pagination kicks in.
func (b *GoogleBackend) ListQuery(ctx context.Context, prefix string) ([]string, error) {
	if err := b.checkCircuitBreaker(); err != nil {
		return nil, err
	}
	tok, err := b.getValidToken(ctx)
	if err != nil {
		return nil, err
	}

	// OPT: Added trashed=false for explicitness (Drive defaults to this, but
	// being explicit may help server-side query planning).
	q := fmt.Sprintf("name contains '%s' and trashed=false", prefix)
	if b.folderID != "" {
		q += fmt.Sprintf(" and '%s' in parents", b.folderID)
	}

	var allNames []string
	pageToken := ""

	for {
		u, _ := url.Parse("https://www.googleapis.com/drive/v3/files")
		v := u.Query()
		v.Set("q", q)
		v.Set("fields", "nextPageToken,files(id,name)")
		v.Set("pageSize", "1000") // Drive maximum
		if pageToken != "" {
			v.Set("pageToken", pageToken)
		}
		u.RawQuery = v.Encode()

		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)

		resp, err := b.httpClient.Do(req)
		if err != nil {
			b.recordFailure()
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b.recordFailure()
			if resp.StatusCode == 429 || resp.StatusCode == 403 {
				return nil, &QuotaError{StatusCode: resp.StatusCode, Body: string(body)}
			}
			return nil, fmt.Errorf("list returned %d: %s", resp.StatusCode, body)
		}
		b.recordSuccess()

		var resData struct {
			NextPageToken string `json:"nextPageToken"`
			Files         []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"files"`
		}
		if err := json.Unmarshal(body, &resData); err != nil {
			return nil, err
		}

		b.fileIdsMu.Lock()
		for _, f := range resData.Files {
			if strings.HasPrefix(f.Name, prefix) {
				if _, exists := b.fileIDs[f.Name]; !exists {
					b.fileIDOrder = append(b.fileIDOrder, f.Name)
				}
				b.fileIDs[f.Name] = f.ID
				allNames = append(allNames, f.Name)
			}
		}
		// OPT-G10: LRU eviction instead of nuclear map reset.
		// Evict the oldest 25% of entries instead of losing ALL active fileIDs.
		if len(b.fileIDs) > 5000 {
			evictCount := len(b.fileIDOrder) / 4
			if evictCount < 100 {
				evictCount = 100
			}
			if evictCount > len(b.fileIDOrder) {
				evictCount = len(b.fileIDOrder)
			}
			for _, name := range b.fileIDOrder[:evictCount] {
				delete(b.fileIDs, name)
			}
			b.fileIDOrder = b.fileIDOrder[evictCount:]
			log.Printf("OPT-G10: evicted %d oldest fileID entries, %d remain", evictCount, len(b.fileIDs))
		}
		b.fileIdsMu.Unlock()

		if resData.NextPageToken == "" {
			break // All pages consumed
		}
		pageToken = resData.NextPageToken
	}

	return allNames, nil
}

func (b *GoogleBackend) Download(ctx context.Context, filename string) (io.ReadCloser, error) {
	if err := b.checkCircuitBreaker(); err != nil {
		return nil, err
	}

	b.fileIdsMu.RLock()
	fileID, ok := b.fileIDs[filename]
	b.fileIdsMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("file-id mapping not found for %s", filename)
	}

	tok, err := b.getValidToken(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://www.googleapis.com/drive/v3/files/"+fileID+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.recordFailure()
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		b.recordFailure()
		if resp.StatusCode == 429 || resp.StatusCode == 403 {
			return nil, &QuotaError{StatusCode: resp.StatusCode, Body: string(body)}
		}
		return nil, fmt.Errorf("download returned %d: %s", resp.StatusCode, body)
	}
	b.recordSuccess()

	return resp.Body, nil
}

func (b *GoogleBackend) Delete(ctx context.Context, filename string) error {
	if err := b.checkCircuitBreaker(); err != nil {
		return err
	}

	b.fileIdsMu.RLock()
	fileID, ok := b.fileIDs[filename]
	b.fileIdsMu.RUnlock()

	if !ok {
		return fmt.Errorf("file-id mapping not found for %s", filename)
	}

	tok, err := b.getValidToken(ctx)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "DELETE",
		"https://www.googleapis.com/drive/v3/files/"+fileID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.recordFailure()
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		b.recordFailure()
		if resp.StatusCode == 429 || resp.StatusCode == 403 {
			return &QuotaError{StatusCode: resp.StatusCode, Body: string(body)}
		}
		return fmt.Errorf("delete returned %d: %s", resp.StatusCode, body)
	}
	b.recordSuccess()

	b.fileIdsMu.Lock()
	delete(b.fileIDs, filename)
	b.fileIdsMu.Unlock()

	return nil
}

func (b *GoogleBackend) CreateFolder(ctx context.Context, name string) (string, error) {
	tok, err := b.getValidToken(ctx)
	if err != nil {
		return "", err
	}

	meta := map[string]interface{}{
		"name":     name,
		"mimeType": "application/vnd.google-apps.folder",
	}
	body, _ := json.Marshal(meta)

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://www.googleapis.com/drive/v3/files", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		resBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create folder returned %d: %s", resp.StatusCode, resBody)
	}

	var resData struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resData); err != nil {
		return "", err
	}

	b.folderID = resData.ID
	return resData.ID, nil
}

func (b *GoogleBackend) FindFolder(ctx context.Context, name string) (string, error) {
	tok, err := b.getValidToken(ctx)
	if err != nil {
		return "", err
	}

	q := fmt.Sprintf("name = '%s' and mimeType = 'application/vnd.google-apps.folder' and trashed = false", name)
	u, _ := url.Parse("https://www.googleapis.com/drive/v3/files")
	v := u.Query()
	v.Set("q", q)
	v.Set("fields", "files(id,name)")
	u.RawQuery = v.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("find folder returned %d: %s", resp.StatusCode, body)
	}

	var resData struct {
		Files []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resData); err != nil {
		return "", err
	}

	if len(resData.Files) > 0 {
		b.folderID = resData.Files[0].ID
		return resData.Files[0].ID, nil
	}
	return "", nil
}
