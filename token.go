package main

// Cline access tokens live for one hour, so the plugin cannot wait to be asked
// for a refresh: CPA only schedules credentials whose provider registers a
// refresh lead, and plugin-owned providers register none. A plugin that relies
// on the host scheduler therefore goes dark 60 minutes after login.
//
// This file keeps credentials fresh on the plugin's own initiative. Every
// upstream call runs through a credential that refreshes shortly before expiry,
// retries once after an upstream 401, and persists the rotated pair through
// host.auth.save so the host record, and any later restart, see the new token.
//
// Cline rotates the refresh token on every grant, so the single-flight guard is
// a correctness requirement rather than an optimization: two concurrent
// refreshes would let the second present an already-invalidated token.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// credentialRefreshLead is how early before expiry a token is replaced. Chat
// requests routinely run for minutes, so the lead has to cover an in-flight
// turn, not just the moment the request starts.
const credentialRefreshLead = 10 * time.Minute

// authPathAttribute is the host attribute carrying the physical auth file path.
const authPathAttribute = "path"

// authRefreshIntervalAttribute is the host attribute that makes CPA schedule a
// plugin credential for refresh at all: plugin-owned providers register no
// refresh lead of their own, so without a declared cadence the host never calls
// AuthRefresh and a one-hour Cline token simply expires in place.
const authRefreshIntervalAttribute = "refresh_interval_seconds"

// credential couples a stored credential with the file it persists to and the
// token last presented upstream, so a 401 retry can tell whether refreshing
// actually produced something new.
type credential struct {
	key      string
	path     string
	fileName string
	sa       *storedAuth
	sent     string
	// adopted marks a credential taken from the plugin cache because the host
	// handed over a snapshot that predates the pair we already rotated.
	adopted bool
}

// refreshCall is one in-flight credential refresh that concurrent callers join.
type refreshCall struct {
	done sync.WaitGroup
	sa   *storedAuth
	err  error
}

var (
	// credentialCache holds the newest credential seen per account. The host can
	// hand the executor a StorageJSON snapshot that predates our own save, and
	// without this the next request would refresh all over again using a
	// superseded refresh token.
	credentialCache  = map[string]*storedAuth{}
	credentialMu     sync.Mutex
	credentialCalls  = map[string]*refreshCall{}
	credentialCallMu sync.Mutex
)

// newCredential adopts the newest credential known for the account, preferring
// the plugin's own cache when the host snapshot is older.
func newCredential(sa *storedAuth, attrs map[string]string) *credential {
	c := &credential{sa: sa, fileName: authFileName}
	if sa != nil {
		c.key = credentialKey(sa)
		c.fileName = authFileNameFor(sa)
	}
	if path := strings.TrimSpace(attrs[authPathAttribute]); path != "" {
		c.path = path
		c.fileName = filepath.Base(path)
	}
	if cached := cachedCredential(c.key); cached != nil && sa != nil && cached.Auth.ExpiresAt > sa.Auth.ExpiresAt {
		c.adopted = true
		c.sa = cached
	}
	return c
}

func credentialKey(sa *storedAuth) string {
	if sa == nil {
		return ""
	}
	if id := strings.TrimSpace(sa.Account.ID); id != "" {
		return "id:" + id
	}
	if email := strings.TrimSpace(sa.Account.Email); email != "" {
		return "email:" + strings.ToLower(email)
	}
	return "file:" + authFileNameFor(sa)
}

func cachedCredential(key string) *storedAuth {
	if key == "" {
		return nil
	}
	credentialMu.Lock()
	defer credentialMu.Unlock()
	cached := credentialCache[key]
	if cached == nil {
		return nil
	}
	return cloneStoredAuth(cached)
}

func cacheCredential(sa *storedAuth) {
	if sa == nil {
		return
	}
	key := credentialKey(sa)
	if key == "" {
		return
	}
	credentialMu.Lock()
	credentialCache[key] = cloneStoredAuth(sa)
	credentialMu.Unlock()
}

func cloneStoredAuth(sa *storedAuth) *storedAuth {
	if sa == nil {
		return nil
	}
	copied := *sa
	return &copied
}

// needsRefresh reports whether the credential must be replaced now. An unknown
// expiry counts as stale so a malformed file cannot quietly wedge the account.
func needsRefresh(sa *storedAuth, now time.Time) bool {
	if sa == nil || strings.TrimSpace(sa.Auth.AccessToken) == "" {
		return true
	}
	if sa.Auth.ExpiresAt <= 0 {
		return true
	}
	return !now.Add(credentialRefreshLead).Before(time.UnixMilli(sa.Auth.ExpiresAt))
}

// prepare refreshes the credential when it is close to expiry. force ignores the
// expiry lead, which is what the 401 retry needs.
func (c *credential) prepare(force bool) error {
	if c == nil || c.sa == nil {
		return fmt.Errorf("cline credential is missing")
	}
	if !force && !needsRefresh(c.sa, time.Now()) {
		return nil
	}
	updated, err := refreshCredential(c.key, c.sa)
	if err != nil {
		// A token that is still valid is worth using even though the refresh
		// failed, so only a dead credential stops the request.
		if needsRefresh(c.sa, time.Now()) {
			return err
		}
		log.Printf("cline: token refresh deferred for %s: %v", c.key, err)
		return nil
	}
	c.apply(updated, nil)
	return nil
}

// apply adopts a refreshed credential: it records the new pair, runs optional
// enrichment, then persists once so the host reloads a complete document rather
// than writing twice.
func (c *credential) apply(updated *storedAuth, enrich func(*storedAuth)) {
	if c == nil || updated == nil {
		return
	}
	changed := updated.Auth.AccessToken != c.sa.Auth.AccessToken || updated.Auth.RefreshToken != c.sa.Auth.RefreshToken
	c.sa = updated
	cacheCredential(updated)
	if enrich != nil {
		enrich(updated)
	}
	if changed {
		c.persist()
	}
}

// enrichAndPersist refreshes the account snapshot on a credential the host
// asked us to rotate, then writes once so plan and credits land with the token.
func (c *credential) enrichAndPersist(enrich func(*storedAuth)) {
	if c == nil || c.sa == nil {
		return
	}
	if enrich != nil {
		enrich(c.sa)
	}
	cacheCredential(c.sa)
	c.persist()
}

// freshStoredAuth returns a credential that is safe to use now, refreshing and
// persisting it when the stored token is close to expiry. Housekeeping callers
// (model discovery, account snapshots) use this so a panel opened an hour after
// login does not silently show an empty catalog.
func freshStoredAuth(sa *storedAuth, attrs map[string]string) *storedAuth {
	if sa == nil {
		return nil
	}
	cred := newCredential(sa, attrs)
	if err := cred.prepare(false); err != nil {
		log.Printf("cline: credential %s not refreshable: %v", cred.key, err)
		return sa
	}
	return cred.sa
}

// nextCredentialRefreshAt tells the host when to ask again. It matches the
// plugin's own lead so the two mechanisms cannot disagree about freshness.
func nextCredentialRefreshAt(sa *storedAuth) time.Time {
	if sa == nil || sa.Auth.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(sa.Auth.ExpiresAt).Add(-credentialRefreshLead)
}

// persist writes the credential back without disturbing the host-owned fields
// (type, disabled, note) already in the physical file. Rewriting the document
// from scratch would silently re-enable an account the management UI disabled.
func (c *credential) persist() {
	raw, err := c.fileJSON()
	if err != nil {
		log.Printf("cline: encode credential %s: %v", c.fileName, err)
		return
	}
	name := strings.TrimSpace(c.fileName)
	if name == "" {
		name = authFileNameFor(c.sa)
	}
	if errSave := hostAuthSaveJSON(name, raw); errSave != nil {
		log.Printf("cline: persist credential %s: %v", name, errSave)
	}
}

// fileJSON merges the credential into the document already on disk.
func (c *credential) fileJSON() ([]byte, error) {
	doc := map[string]any{}
	if c.path != "" {
		if disk, errRead := os.ReadFile(c.path); errRead == nil {
			_ = json.Unmarshal(disk, &doc)
		}
	}
	nested, err := json.Marshal(c.sa)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(nested, &fields); err != nil {
		return nil, err
	}
	if len(doc) == 0 {
		return buildAuthFileJSON(c.sa)
	}
	if raw, ok := fields["auth"]; ok {
		doc["auth"] = raw
	}
	if raw, ok := fields["account"]; ok {
		doc["account"] = raw
	}
	if label := clineAccountLabel(c.sa); label != "" {
		doc["label"] = label
	}
	if email := strings.TrimSpace(c.sa.Account.Email); email != "" {
		doc["email"] = email
	}
	return json.Marshal(doc)
}

// refreshCredential refreshes once per account and lets concurrent callers join
// that single flight.
func refreshCredential(key string, sa *storedAuth) (*storedAuth, error) {
	if key == "" {
		key = credentialKey(sa)
	}
	call := &refreshCall{}
	call.done.Add(1)
	credentialCallMu.Lock()
	if existing := credentialCalls[key]; existing != nil {
		credentialCallMu.Unlock()
		existing.done.Wait()
		if existing.err != nil {
			return nil, existing.err
		}
		return cloneStoredAuth(existing.sa), nil
	}
	credentialCalls[key] = call
	credentialCallMu.Unlock()

	call.sa, call.err = requestTokenRefresh(sa)
	if call.err == nil {
		cacheCredential(call.sa)
	}

	credentialCallMu.Lock()
	if credentialCalls[key] == call {
		delete(credentialCalls, key)
	}
	credentialCallMu.Unlock()
	call.done.Done()

	if call.err != nil {
		return nil, call.err
	}
	return cloneStoredAuth(call.sa), nil
}

// requestTokenRefresh exchanges the refresh token for a new pair. It never
// mutates the credential it was given, so a failed grant leaves the caller's
// copy untouched.
func requestTokenRefresh(sa *storedAuth) (*storedAuth, error) {
	if sa == nil {
		return nil, fmt.Errorf("stored auth is nil")
	}
	if strings.TrimSpace(sa.Auth.RefreshToken) == "" {
		return nil, &credentialError{terminal: true, message: "Cline credential has no refresh token; re-login required"}
	}
	payload := map[string]string{
		"refreshToken": sa.Auth.RefreshToken,
		"grantType":    "refresh_token",
	}
	resp, err := postJSON(clineAPIBase+"/api/v1/auth/refresh", payload, nil)
	if err != nil {
		return nil, fmt.Errorf("refresh request failed: %w", err)
	}
	body, err := readAllAndClose(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A 4xx grant refusal means the refresh token itself is dead, which only
		// a re-login fixes; anything else is transient and must not tell the user
		// to authenticate again.
		terminal := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
		message := fmt.Sprintf("Cline token refresh failed: HTTP %d: %s", resp.StatusCode, truncate(string(body), 240))
		if terminal {
			message = "Cline refresh token no longer accepted; re-login required"
		}
		return nil, &credentialError{terminal: terminal, status: resp.StatusCode, message: message}
	}
	var parsed clineAuthResponse
	if err = json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode Cline refresh response: %w", err)
	}
	if !parsed.Success || parsed.Data.AccessToken == "" {
		return nil, &credentialError{terminal: true, message: "Cline refresh returned no access token; re-login required"}
	}
	updated := cloneStoredAuth(sa)
	updated.Auth.AccessToken = parsed.Data.AccessToken
	if parsed.Data.RefreshToken != "" {
		updated.Auth.RefreshToken = parsed.Data.RefreshToken
	}
	updated.Auth.ExpiresAt = parseExpiryMillis(parsed.Data.ExpiresAt)
	if parsed.Data.UserInfo.ClineUID != "" {
		updated.Account.ID = parsed.Data.UserInfo.ClineUID
	}
	if parsed.Data.UserInfo.Email != "" {
		updated.Account.Email = parsed.Data.UserInfo.Email
	}
	if parsed.Data.UserInfo.Name != "" {
		updated.Account.DisplayName = parsed.Data.UserInfo.Name
	}
	log.Printf("cline: refreshed credential %s (expires %s)", credentialKey(updated),
		time.UnixMilli(updated.Auth.ExpiresAt).Format(time.RFC3339))
	return updated, nil
}

// credentialError separates a dead credential from a transient refresh failure.
// The two need different status codes, and only one of them should take the
// account out of rotation.
type credentialError struct {
	terminal bool
	status   int
	message  string
}

func (e *credentialError) Error() string { return e.message }

// do sends a request built from the current credential. It refreshes up front
// when the token is close to expiry and once more after an upstream 401, so a
// token revoked server side still completes the user's request.
func (c *credential) do(build func(*storedAuth) (*http.Request, error)) (*http.Response, error) {
	var lastBody []byte
	for attempt := 0; attempt < 2; attempt++ {
		// Only the first attempt decides freshness on its own. A second attempt
		// always follows the forced refresh below, and refreshing again there
		// would burn a freshly rotated token on every 401.
		if attempt == 0 {
			if err := c.prepare(false); err != nil {
				return nil, credentialFailure(err)
			}
		}
		httpReq, err := build(c.sa)
		if err != nil {
			return nil, err
		}
		c.sent = c.sa.Auth.AccessToken
		resp, errDo := sharedHTTPClient().Do(httpReq)
		if errDo != nil {
			return nil, fmt.Errorf("http_error: %w", errDo)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
		body, _ := readAllAndClose(resp.Body)
		lastBody = body
		if err := c.prepare(true); err != nil {
			return nil, credentialFailure(err)
		}
		// Only retry when the credential actually changed; the same token would
		// earn the same 401.
		if c.sa.Auth.AccessToken == c.sent {
			return nil, clineUpstreamError(http.StatusUnauthorized, lastBody)
		}
	}
	return nil, clineUpstreamError(http.StatusUnauthorized, lastBody)
}

// credentialFailure maps a refresh failure onto a status the host can act on:
// 401 only when the credential itself is dead, 503 for a transient grant error.
func credentialFailure(err error) error {
	var credErr *credentialError
	if errors.As(err, &credErr) {
		if credErr.terminal {
			return &upstreamStatusError{status: http.StatusUnauthorized, message: credErr.message}
		}
		return &upstreamStatusError{status: http.StatusServiceUnavailable, message: credErr.message}
	}
	// A refresh transport failure says nothing about the credential itself.
	// Keep it status-less and use the lifecycle wording CPA recognizes so the
	// host does not turn a transient network fault into a model/auth cooldown.
	return fmt.Errorf("cline credential refresh failed (unexpected EOF): %w", err)
}
