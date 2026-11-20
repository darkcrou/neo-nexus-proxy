// SPDX-License-Identifier: Apache-2.0

// Package qwen manages a NEXUS-owned Qwen Code subscription session.
package qwen

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	ProviderType = "qwen-subscription"
	GatewayURL   = "https://api.qwen.ai/compatible-mode/v1"
	authURL      = "https://auth.qwen.ai"
	refreshSkew  = 30 * time.Second
)

var (
	ErrSignInRequired = errors.New("Qwen subscription needs sign-in. Use Connect Qwen or run nexus auth qwen-subscription")
	ErrMembership     = errors.New("this Qwen account has no eligible Code subscription")
	ErrSuperseded     = errors.New("a newer Qwen sign-in replaced this attempt")
	ErrExpired        = errors.New("Qwen sign-in expired. Start a new sign-in")
	ErrDenied         = errors.New("Qwen sign-in was denied")
	ErrRefreshLimited = errors.New("Qwen is rate-limiting token refreshes")
	ErrRefresh        = errors.New("Qwen token refresh is temporarily unavailable")
)

// Service uses a private database shared by NEXUS processes on the same host.
// A write transaction reserves the session before a refresh uses its grant.
type Service struct {
	db     *sql.DB
	client *http.Client
	origin string
	now    func() time.Time
}

func Open(stateDir string) (*Service, error) {
	dir, err := filepath.Abs(filepath.Join(stateDir, "qwen"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("Qwen state path must be a directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "session.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("Qwen session path must be a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}

	dbPath := filepath.ToSlash(path)
	if dbPath[0] != '/' {
		dbPath = "/" + dbPath
	}
	dsn := url.URL{
		Scheme:   "file",
		Path:     dbPath,
		RawQuery: "_txlock=immediate&_pragma=busy_timeout(12000)&_pragma=synchronous(FULL)",
	}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS qwen_session (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			attempt TEXT NOT NULL DEFAULT '',
			revision TEXT NOT NULL DEFAULT '',
			access_token TEXT NOT NULL DEFAULT '',
			refresh_token TEXT NOT NULL DEFAULT '',
			expires_at INTEGER NOT NULL DEFAULT 0,
			retry_at INTEGER NOT NULL DEFAULT 0,
			refresh_failure TEXT NOT NULL DEFAULT ''
		);
		INSERT OR IGNORE INTO qwen_session (id) VALUES (1);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize Qwen session: %w", err)
	}
	return &Service{
		db: db, origin: authURL, now: time.Now,
		client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: rejectRedirect},
	}, nil
}

func (s *Service) Close() error { return s.db.Close() }

// Status contains only local session information safe for the dashboard.
type Status struct {
	Revision  string
	Connected bool
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	var revision string
	var connected bool
	err := s.db.QueryRowContext(ctx, `
		SELECT revision, refresh_token <> '' AND refresh_failure <> 'sign_in'
		FROM qwen_session WHERE id = 1
	`).Scan(&revision, &connected)
	return Status{Revision: revision, Connected: connected}, err
}

type credentials struct {
	access, refresh, failure string
	expiresAt, retryAt       int64
}

type rowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readCredentials(ctx context.Context, db rowReader) (credentials, error) {
	var c credentials
	err := db.QueryRowContext(ctx, `
		SELECT access_token, refresh_token, expires_at, retry_at, refresh_failure
		FROM qwen_session WHERE id = 1
	`).Scan(&c.access, &c.refresh, &c.expiresAt, &c.retryAt, &c.failure)
	return c, err
}

func (c credentials) valid(now time.Time, rejected string) bool {
	return c.access != "" && c.access != rejected && now.Add(refreshSkew).UnixMilli() < c.expiresAt
}

// Token returns a current bearer token. Pass the exact token used on a rejected
// request so concurrent callers can adopt the new token without rotating again.
func (s *Service) Token(ctx context.Context, rejected string) (string, error) {
	c, err := readCredentials(ctx, s.db)
	if err != nil {
		return "", err
	}
	switch {
	case c.refresh == "" || c.failure == "sign_in":
		return "", ErrSignInRequired
	case c.valid(s.now(), rejected):
		return c.access, nil
	}
	// Once rotation starts, finish persisting the result even when the original
	// request times out. Qwen may already have invalidated the old refresh token.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 22*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	c, err = readCredentials(ctx, tx)
	if err != nil {
		return "", err
	}
	now := s.now()
	switch {
	case c.refresh == "" || c.failure == "sign_in":
		return "", ErrSignInRequired
	case c.valid(now, rejected):
		return c.access, nil
	case now.UnixMilli() < c.retryAt && c.failure == "rate":
		return "", ErrRefreshLimited
	case now.UnixMilli() < c.retryAt:
		return "", ErrRefresh
	}
	var tokens tokenResponse
	err = s.form(ctx, "/api/oauth/token", url.Values{
		"client_id": {clientID}, "grant_type": {"refresh_token"}, "refresh_token": {c.refresh},
	}, &tokens)
	if err != nil {
		return "", s.recordRefreshFailure(ctx, tx, err)
	}
	expiry, err := tokens.expiry(s.now())
	if err != nil {
		return "", s.recordRefreshFailure(ctx, tx, err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE qwen_session SET access_token = ?, refresh_token = ?, expires_at = ?,
		retry_at = 0, refresh_failure = '' WHERE id = 1
	`, tokens.Access, tokens.Refresh, expiry)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return tokens.Access, nil
}

func (s *Service) recordRefreshFailure(ctx context.Context, tx *sql.Tx, cause error) error {
	failure, retryAt, result := "transient", s.now().Add(5*time.Second).UnixMilli(), ErrRefresh
	var response *oauthError
	if errors.As(cause, &response) {
		if response.retryAfter > 0 {
			retryAt = s.now().Add(response.retryAfter).UnixMilli()
		}
		switch {
		case response.code == "invalid_grant" || response.code == "access_denied" || response.code == "expired_token":
			failure, retryAt, result = "sign_in", int64(0), ErrSignInRequired
		case response.status == http.StatusTooManyRequests:
			delay := response.retryAfter
			if delay <= 0 {
				delay = 30 * time.Second
			}
			failure, retryAt, result = "rate", s.now().Add(min(delay, 24*time.Hour)).UnixMilli(), ErrRefreshLimited
		}
	}
	_, err := tx.ExecContext(ctx, "UPDATE qwen_session SET retry_at = ?, refresh_failure = ? WHERE id = 1", retryAt, failure)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return result
}

// Flow exposes the user-facing authorization data. The device code stays private.
type Flow struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	UserCode   string    `json:"user_code"`
	ExpiresAt  time.Time `json:"expires_at"`
	deviceCode string
	interval   time.Duration
}

func (s *Service) BeginLogin(ctx context.Context) (Flow, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Flow{}, err
	}
	id := hex.EncodeToString(random[:])
	if _, err := s.db.ExecContext(ctx, "UPDATE qwen_session SET attempt = ? WHERE id = 1", id); err != nil {
		return Flow{}, err
	}
	flow, err := s.requestDeviceCode(ctx, id)
	if err != nil {
		s.clearAttempt(ctx, id)
	}
	return flow, err
}

func (s *Service) clearAttempt(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 13*time.Second)
	defer cancel()
	s.db.ExecContext(ctx, "UPDATE qwen_session SET attempt = '' WHERE id = 1 AND attempt = ?", id)
}

// FinishLogin polls the device grant and commits it only if the attempt is still current.
func (s *Service) FinishLogin(ctx context.Context, flow Flow) error {
	defer s.clearAttempt(ctx, flow.ID)
	tokens, err := s.pollDeviceCode(ctx, flow)
	if err != nil {
		return err
	}
	expires, err := tokens.expiry(s.now())
	if err != nil {
		return err
	}
	if err := s.checkMembership(ctx, tokens.Access); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !s.now().Before(flow.ExpiresAt) {
		return ErrExpired
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE qwen_session SET revision = ?, attempt = '', access_token = ?, refresh_token = ?,
		expires_at = ?, retry_at = 0, refresh_failure = '' WHERE id = 1 AND attempt = ?
	`, flow.ID, tokens.Access, tokens.Refresh, expires, flow.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrSuperseded
	}
	return tx.Commit()
}
