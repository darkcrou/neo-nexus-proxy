// SPDX-License-Identifier: Apache-2.0

package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const clientID = "qwen-code-cli"

type oauthError struct {
	code       string
	status     int
	retryAfter time.Duration
}

func (e *oauthError) Error() string {
	return fmt.Sprintf("Qwen authorization request failed (HTTP %d)", e.status)
}

func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (s *Service) form(ctx context.Context, path string, fields url.Values, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.origin+path, strings.NewReader(fields.Encode()))
	if err != nil {
		return errors.New("prepare Qwen authorization request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return s.send(req, output)
}

func (s *Service) send(req *http.Request, output any) error {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "NEXUS")
	resp, err := s.client.Do(req)
	if err != nil {
		if err := req.Context().Err(); err != nil {
			return err
		}
		return errors.New("could not reach Qwen authorization service")
	}
	defer resp.Body.Close()
	const limit = 256 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return errors.New("could not read Qwen authorization response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var payload struct {
			Error string `json:"error"`
		}
		if len(body) <= limit {
			_ = json.Unmarshal(body, &payload)
		}
		return &oauthError{code: payload.Error, status: resp.StatusCode, retryAfter: retryAfter(resp.Header.Get("Retry-After"), s.now())}
	}
	if len(body) > limit || json.Unmarshal(body, output) != nil {
		return errors.New("Qwen returned an invalid authorization response")
	}
	return nil
}

func retryAfter(header string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 86400)) * time.Second
	}
	if until, err := http.ParseTime(header); err == nil && until.After(now) {
		return min(until.Sub(now), 24*time.Hour)
	}
	return 0
}

type tokenResponse struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	Type      string `json:"token_type"`
	ExpiresIn int64  `json:"expires_in"`
}

func (t tokenResponse) expiry(now time.Time) (int64, error) {
	if t.Access == "" || t.Refresh == "" || !strings.EqualFold(t.Type, "Bearer") || t.ExpiresIn <= 0 || t.ExpiresIn > 86400*365 {
		return 0, errors.New("Qwen returned an invalid token pair")
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second).UnixMilli(), nil
}

func (s *Service) requestDeviceCode(ctx context.Context, id string) (Flow, error) {
	var code struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		ExpiresIn  int64  `json:"expires_in"`
		Interval   int64  `json:"interval"`
	}
	err := s.form(ctx, "/api/oauth/device/code", url.Values{
		"client_id": {clientID}, "scope": {"model:invoke profile:read"},
	}, &code)
	if err != nil {
		return Flow{}, err
	}
	if code.DeviceCode == "" || code.UserCode == "" || code.ExpiresIn <= 0 || code.ExpiresIn > 86400 {
		return Flow{}, errors.New("Qwen returned an invalid device authorization")
	}
	interval := code.Interval
	if interval <= 0 {
		interval = 5
	}
	params := url.Values{
		"user_code": {code.UserCode}, "client": {"cli"}, "entry": {"qwen_code"}, "paywallScene": {"cli"},
	}
	return Flow{
		ID: id, URL: "https://chat.qwen.ai/authorize?" + params.Encode(), UserCode: code.UserCode,
		ExpiresAt:  s.now().Add(time.Duration(code.ExpiresIn) * time.Second),
		deviceCode: code.DeviceCode, interval: time.Duration(min(interval, code.ExpiresIn)) * time.Second,
	}, nil
}

func (s *Service) pollDeviceCode(ctx context.Context, flow Flow) (tokenResponse, error) {
	ctx, cancel := context.WithDeadline(ctx, flow.ExpiresAt)
	defer cancel()
	interval, delay := flow.interval, flow.interval
	for {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			if !s.now().Before(flow.ExpiresAt) {
				return tokenResponse{}, ErrExpired
			}
			return tokenResponse{}, ctx.Err()
		}
		var active string
		if err := s.db.QueryRowContext(ctx, "SELECT attempt FROM qwen_session WHERE id = 1").Scan(&active); err != nil {
			return tokenResponse{}, err
		}
		if active != flow.ID {
			return tokenResponse{}, ErrSuperseded
		}
		var tokens tokenResponse
		err := s.form(ctx, "/api/oauth/token", url.Values{
			"client_id": {clientID}, "device_code": {flow.deviceCode},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		}, &tokens)
		if err == nil {
			return tokens, nil
		}
		var response *oauthError
		if !errors.As(err, &response) {
			if ctx.Err() != nil {
				if !s.now().Before(flow.ExpiresAt) {
					return tokenResponse{}, ErrExpired
				}
				return tokenResponse{}, ctx.Err()
			}
			delay = min(max(delay*2, interval), time.Minute)
			continue
		}
		switch {
		case response.code == "authorization_pending":
			delay = max(interval, response.retryAfter)
		case response.code == "slow_down":
			interval += 5 * time.Second
			delay = max(interval, response.retryAfter)
		case response.code == "expired_token":
			return tokenResponse{}, ErrExpired
		case response.code == "access_denied":
			return tokenResponse{}, ErrDenied
		case response.status == http.StatusTooManyRequests || response.status >= 500:
			delay = max(min(delay*2, time.Minute), interval, response.retryAfter)
		default:
			return tokenResponse{}, err
		}
	}
}

func (s *Service) checkMembership(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.origin+"/api/oauth/userinfo", nil)
	if err != nil {
		return errors.New("prepare Qwen membership request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var user struct {
		Subject    string `json:"sub"`
		Membership string `json:"member_type"`
	}
	if err := s.send(req, &user); err != nil {
		var response *oauthError
		if errors.As(err, &response) && response.status == http.StatusForbidden {
			return ErrMembership
		}
		return err
	}
	if user.Subject == "" || user.Membership == "" {
		return errors.New("Qwen returned an invalid membership response")
	}
	switch strings.ToLower(user.Membership) {
	case "plus", "pro":
		return nil
	default:
		return ErrMembership
	}
}
