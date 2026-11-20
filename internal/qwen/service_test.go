package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openTestService(t *testing.T, dir string, server *httptest.Server) *Service {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.origin = server.URL
	return s
}

func writeTestTokens(w http.ResponseWriter, access, refresh string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 900,
	})
}

func signInServer(t *testing.T, refresh func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/device/code":
			if r.Method != http.MethodPost || r.FormValue("client_id") != clientID || r.FormValue("scope") != "model:invoke profile:read" {
				t.Error("wrong device request")
			}
			fmt.Fprint(w, `{"device_code":"device-secret","user_code":"test-code","expires_in":60,"interval":1}`)
		case "/api/oauth/token":
			if r.FormValue("grant_type") == "refresh_token" {
				refresh(w, r)
				return
			}
			if r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.FormValue("device_code") != "device-secret" {
				t.Error("wrong device exchange request")
			}
			writeTestTokens(w, "first-access", "first-refresh")
		case "/api/oauth/userinfo":
			if r.Header.Get("Authorization") != "Bearer first-access" {
				t.Error("wrong userinfo auth")
			}
			fmt.Fprint(w, `{"sub":"test-member","member_type":"plus"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func beginForTest(t *testing.T, s *Service) Flow {
	t.Helper()
	flow, err := s.BeginLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	flow.interval = time.Millisecond
	return flow
}

func TestLoginAndConcurrentRefreshAcrossStores(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	server := signInServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 || r.FormValue("refresh_token") != "first-refresh" {
			t.Error("the first grant was used more than once")
		}
		time.Sleep(30 * time.Millisecond)
		writeTestTokens(w, "next-access", "next-refresh")
	})
	dir := t.TempDir()
	s := openTestService(t, dir, server)
	second := openTestService(t, dir, server)
	oldAttempt := beginForTest(t, s)
	flow := beginForTest(t, second)
	wire, _ := json.Marshal(flow)
	if strings.Contains(string(wire), "device-secret") || !strings.Contains(flow.URL, "user_code=test-code") {
		t.Fatal("unsafe or invalid authorization challenge")
	}
	if err := s.FinishLogin(ctx, oldAttempt); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("old attempt = %v", err)
	}
	if err := second.FinishLogin(ctx, flow); err != nil {
		t.Fatal(err)
	}
	if status, err := s.Status(ctx); err != nil || !status.Connected || status.Revision != flow.ID {
		t.Fatalf("unexpected status: %+v, %v", status, err)
	}

	var wg sync.WaitGroup
	for _, store := range []*Service{s, second, s, second, s} {
		wg.Add(1)
		go func(store *Service) {
			defer wg.Done()
			token, err := store.Token(ctx, "first-access")
			if err != nil || token != "next-access" {
				t.Errorf("expected replacement token, got error %v", err)
			}
		}(store)
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d", calls.Load())
	}
	reopened := openTestService(t, dir, server)
	c, err := readCredentials(ctx, reopened.db)
	if err != nil || c.access != "next-access" || c.refresh != "next-refresh" {
		t.Fatal("session did not persist the complete rotated pair", err)
	}
}

func TestRefreshCooldownAndDeadGrant(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	server := signInServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "45")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"rate_limited"}`)
		} else {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"private value"}`)
		}
	})
	dir := t.TempDir()
	s := openTestService(t, dir, server)
	if err := s.FinishLogin(ctx, beginForTest(t, s)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx, "first-access"); !errors.Is(err, ErrRefreshLimited) {
		t.Fatalf("first refresh: %v", err)
	}
	reopened := openTestService(t, dir, server)
	if _, err := reopened.Token(ctx, "first-access"); !errors.Is(err, ErrRefreshLimited) || calls.Load() != 1 {
		t.Fatalf("refresh during persisted cooldown: %v, calls %d", err, calls.Load())
	}
	reopened.now = func() time.Time { return time.Now().Add(time.Minute) }
	for range 2 {
		if _, err := reopened.Token(ctx, "first-access"); !errors.Is(err, ErrSignInRequired) {
			t.Fatalf("dead grant: %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("refresh calls = %d", calls.Load())
	}
	if status, err := s.Status(ctx); err != nil || status.Connected {
		t.Fatalf("unexpected status: %+v, %v", status, err)
	}
	if err := s.FinishLogin(ctx, beginForTest(t, s)); err != nil {
		t.Fatal(err)
	}
	if token, err := s.Token(ctx, ""); err != nil || token != "first-access" {
		t.Fatalf("replacement login not picked up: %v", err)
	}
}
