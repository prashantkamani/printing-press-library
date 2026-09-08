// Copyright 2026 Aria Kamani and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Tests for the hand-authored Garmin auth flow. Nothing here opens a browser
// or contacts Garmin: every server is an httptest instance on loopback, every
// token is a synthetic RS256-shaped JWT with no signature, and every identity
// is a placeholder. Each test builds its own t.TempDir home and leaves no
// state behind.

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/printing-press-library/library/health/garmin/internal/cliutil"
	"github.com/mvanhorn/printing-press-library/library/health/garmin/internal/config"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fakeJWT builds an unsigned token whose header claims RS256 (the algorithm a
// real DI access token uses) and whose payload carries claims. The signature
// segment is a placeholder: nothing in this CLI verifies it, and nothing here
// should be mistaken for a credential.
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt segment: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test"}) + "." + enc(claims) + ".not-a-signature"
}

func endpointsFor(server *httptest.Server) garminEndpoints {
	return garminEndpoints{
		Domain:     garminDomainGlobal,
		SSOSignin:  server.URL + "/sso/signin",
		SSOLogout:  server.URL + "/sso/logout",
		DITokenURL: server.URL + "/di-oauth2-service/oauth/token",
		ConnectAPI: server.URL,
	}
}

// ---------------------------------------------------------------------------
// Functional: loopback state and ticket handling
// ---------------------------------------------------------------------------

func TestCallbackDeliversTicketWithMatchingState(t *testing.T) {
	state, err := garminNewState()
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	if len(state) != 32 {
		t.Fatalf("state nonce = %d hex chars, want 32 (128 bits)", len(state))
	}
	cb, err := garminStartCallback(state)
	if err != nil {
		t.Fatalf("start callback: %v", err)
	}
	defer func() { _ = cb.Close() }()

	if !strings.HasPrefix(cb.URL, "http://127.0.0.1:") {
		t.Fatalf("callback bound to %q, want a 127.0.0.1 port", cb.URL)
	}
	parsed, err := url.Parse(cb.URL)
	if err != nil {
		t.Fatalf("parse callback url: %v", err)
	}
	if parsed.Query().Get("state") != state {
		t.Fatalf("callback URL does not carry the state nonce: %s", cb.URL)
	}

	resp, err := http.Get(cb.URL + "&ticket=ST-fixture-0001") // #nosec G107 -- loopback test server
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", resp.StatusCode)
	}

	select {
	case ticket := <-cb.Ticket():
		if ticket != "ST-fixture-0001" {
			t.Fatalf("ticket = %q, want ST-fixture-0001", ticket)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ticket delivered")
	}
	if cb.Rejected() != 0 {
		t.Fatalf("rejected = %d, want 0", cb.Rejected())
	}
}

// Negative: a callback carrying the wrong nonce is refused and never delivers
// a ticket, so a foreign page that guesses the port cannot inject one.
func TestCallbackRefusesForeignState(t *testing.T) {
	state, err := garminNewState()
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	cb, err := garminStartCallback(state)
	if err != nil {
		t.Fatalf("start callback: %v", err)
	}
	defer func() { _ = cb.Close() }()

	base := strings.SplitN(cb.URL, "?", 2)[0]
	resp, err := http.Get(base + "?state=wrong&ticket=ST-attacker") // #nosec G107 -- loopback test server
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign state status = %d, want 400", resp.StatusCode)
	}
	select {
	case ticket := <-cb.Ticket():
		t.Fatalf("a foreign-state callback delivered ticket %q", ticket)
	case <-time.After(200 * time.Millisecond):
	}
	if cb.Rejected() != 1 {
		t.Fatalf("rejected = %d, want 1", cb.Rejected())
	}

	// A ticket-less hit (favicon, health probe) is neither delivered nor
	// counted as a rejection.
	resp2, err := http.Get(base) // #nosec G107 -- loopback test server
	if err != nil {
		t.Fatalf("bare GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("ticket-less status = %d, want 204", resp2.StatusCode)
	}
	if cb.Rejected() != 1 {
		t.Fatalf("rejected after a ticket-less hit = %d, want 1", cb.Rejected())
	}
}

// Negative: the listener accepts one ticket. A replay gets 409 and cannot
// overwrite the delivered one.
func TestCallbackAcceptsOnlyOneTicket(t *testing.T) {
	state, _ := garminNewState()
	cb, err := garminStartCallback(state)
	if err != nil {
		t.Fatalf("start callback: %v", err)
	}
	defer func() { _ = cb.Close() }()

	for i, want := range []int{http.StatusOK, http.StatusConflict} {
		resp, err := http.Get(cb.URL + fmt.Sprintf("&ticket=ST-%d", i)) // #nosec G107 -- loopback test server
		if err != nil {
			t.Fatalf("callback GET %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("callback %d status = %d, want %d", i, resp.StatusCode, want)
		}
	}
}

func TestBuildSSOURLCarriesLoopbackAndEmail(t *testing.T) {
	ep := garminNewEndpoints(garminDomainGlobal)
	raw := garminBuildSSOURL(ep, "http://127.0.0.1:54321/callback?state=abc", "person@example.test")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse sso url: %v", err)
	}
	q := parsed.Query()
	for _, key := range []string{"service", "source", "redirectAfterAccountLoginUrl", "redirectAfterAccountCreationUrl"} {
		if q.Get(key) != "http://127.0.0.1:54321/callback?state=abc" {
			t.Fatalf("%s = %q, want the loopback callback URL", key, q.Get(key))
		}
	}
	if q.Get("prepopUsername") != "person@example.test" {
		t.Fatalf("prepopUsername = %q", q.Get("prepopUsername"))
	}
	if !strings.HasPrefix(raw, "https://sso.garmin.com/sso/signin?") {
		t.Fatalf("sso url = %q, want the sso.garmin.com sign-in page", raw)
	}
}

func TestLogoutRouteIsTheCASLogoutPath(t *testing.T) {
	ep := garminNewEndpoints(garminDomainGlobal)
	if ep.SSOLogout != "https://sso.garmin.com/sso/logout" {
		t.Fatalf("logout route = %q, want https://sso.garmin.com/sso/logout", ep.SSOLogout)
	}
}

// Negative: garminCheckDomain admits only Garmin's two deployments, so a
// stored or flag-supplied domain cannot name a third host. (The separate
// GARMIN_TOKEN_URL override is covered by TestTokenURLOverrideIsLoopbackOnly.)
func TestDomainAllowlist(t *testing.T) {
	for _, ok := range []string{garminDomainGlobal, garminDomainChina} {
		if err := garminCheckDomain(ok); err != nil {
			t.Fatalf("domain %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "garmin.com.evil.test", "example.test", "GARMIN.COM"} {
		if err := garminCheckDomain(bad); err == nil {
			t.Fatalf("domain %q accepted, want refusal", bad)
		}
	}
}

// Negative: the refresh POST carries the refresh token in its body, so the
// token-service override must not be able to name an arbitrary host.
func TestTokenURLOverrideIsLoopbackOnly(t *testing.T) {
	cfg := &config.Config{BaseURL: "https://connectapi.garmin.com"}
	defaultURL := garminNewEndpoints(garminDomainGlobal).DITokenURL

	t.Run("loopback accepted", func(t *testing.T) {
		t.Setenv("GARMIN_TOKEN_URL", "http://127.0.0.1:8123/token")
		if got := garminEndpointsFor(cfg, garminDomainGlobal).DITokenURL; got != "http://127.0.0.1:8123/token" {
			t.Fatalf("loopback override not honoured: %q", got)
		}
	})
	t.Run("localhost accepted", func(t *testing.T) {
		t.Setenv("GARMIN_TOKEN_URL", "http://localhost:8123/token")
		if got := garminEndpointsFor(cfg, garminDomainGlobal).DITokenURL; got != "http://localhost:8123/token" {
			t.Fatalf("localhost override not honoured: %q", got)
		}
	})
	for name, raw := range map[string]string{
		"remote host":      "https://collector.example.test/token",
		"garmin-lookalike": "https://diauth.garmin.com.example.test/token",
		"not a url":        "::::",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GARMIN_TOKEN_URL", raw)
			if got := garminEndpointsFor(cfg, garminDomainGlobal).DITokenURL; got != defaultURL {
				t.Fatalf("override %q was accepted (%q); only loopback may redirect the token service", raw, got)
			}
		})
	}
}

// The --home rung moves the whole home, which is how two Garmin accounts stay
// separate. Requirement: auth status reports the rung it resolved.
func TestHomeRungReportsTheHomeFlagRung(t *testing.T) {
	home := t.TempDir()
	restore, err := cliutil.SetHomeOverride(home)
	if err != nil {
		t.Fatalf("set home override: %v", err)
	}
	defer restore()

	rung, dir, source := garminHomeRung()
	if rung != "--home" {
		t.Fatalf("home rung = %q, want --home", rung)
	}
	if !strings.HasPrefix(dir, home) {
		t.Fatalf("config dir = %q, want it under %q", dir, home)
	}
	if source != "--home" {
		t.Fatalf("home source = %q, want --home", source)
	}
	path, err := garminResolveConfigPath("")
	if err != nil {
		t.Fatalf("resolve config path: %v", err)
	}
	if !strings.HasPrefix(path, home) {
		t.Fatalf("resolved config path = %q, want it under the --home root", path)
	}
}

// auth logout clears both halves of a home's state: the credentials the
// generated layer stores and the identity sidecar this CLI adds.
func TestAuthLogoutClearsCredentialsAndIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GARMIN_HOME", home)
	configPath := filepath.Join(home, "config", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("make config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("base_url = 'https://connectapi.garmin.com'\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	access := fakeJWT(t, map[string]any{"exp": time.Now().Add(20 * time.Hour).Unix()})
	if err := cfg.SaveTokens("GARMIN_TEST_CLIENT", "", access, "refresh-fixture", time.Now().Add(20*time.Hour)); err != nil {
		t.Fatalf("seed tokens: %v", err)
	}
	if err := garminSaveIdentity(cfg.Path, &garminIdentity{Email: "placeholder@example.test", Domain: garminDomainGlobal}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	if reloaded, err := config.Load(""); err != nil || reloaded.AuthHeader() == "" {
		t.Fatalf("precondition: the seeded home should be authenticated (err=%v)", err)
	}

	root := RootCmd()
	root.SetArgs([]string{"auth", "logout"})
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("auth logout: %v\n%s", err, out.String())
	}

	after, err := config.Load("")
	if err != nil {
		t.Fatalf("load after logout: %v", err)
	}
	if after.AuthHeader() != "" {
		t.Fatal("auth logout left a usable credential behind")
	}
	if after.AccessToken != "" || after.RefreshToken != "" {
		t.Fatal("auth logout left token fields populated")
	}
	if _, err := garminLoadIdentity(cfg.Path); !os.IsNotExist(err) {
		t.Fatalf("auth logout left the identity sidecar in place (err=%v)", err)
	}
	if !strings.Contains(out.String(), "Signed out") {
		t.Fatalf("auth logout said nothing useful: %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// Functional + negative: token exchange and refresh wire shapes
// ---------------------------------------------------------------------------

// The refresh request carries client_id and the refresh token in the body and
// nothing else: no Authorization header, no native app headers. That is the
// shape proven accepted against Garmin on 2026-09-07 and the shape the
// printing-press generated refresh would use.
func TestRefreshSendsBodyOnlyWithoutBasicHeader(t *testing.T) {
	var (
		gotAuth      string
		gotUA        string
		gotXGarminUA string
		gotForm      url.Values
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotXGarminUA = r.Header.Get("X-Garmin-User-Agent")
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		gotForm, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token_type":"Bearer","access_token":"` +
			fakeJWT(t, map[string]any{"client_id": "GARMIN_TEST_CLIENT", "garmin_guid": "11111111-2222-3333-4444-555555555555", "exp": time.Now().Add(time.Hour).Unix()}) +
			`","refresh_token":"rotated-refresh-fixture","expires_in":67680,"refresh_token_expires_in":2591999}`))
	}))
	defer server.Close()

	old := &garminTokens{
		AccessToken:  fakeJWT(t, map[string]any{"client_id": "GARMIN_TEST_CLIENT", "exp": time.Now().Add(time.Minute).Unix()}),
		RefreshToken: "original-refresh-fixture",
		ClientID:     "GARMIN_TEST_CLIENT",
	}
	next, err := garminRefreshTokens(context.Background(), garminHTTPClient(), endpointsFor(server), old)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if gotAuth != "" {
		t.Fatalf("refresh sent an Authorization header (%q); the proven shape sends none", gotAuth)
	}
	if strings.Contains(gotUA, "GCM-Android") || gotXGarminUA != "" {
		t.Fatalf("refresh sent native app headers (UA=%q, X-Garmin-User-Agent=%q); the proven shape sends none", gotUA, gotXGarminUA)
	}
	if got := gotForm.Get("grant_type"); got != "refresh_token" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := gotForm.Get("client_id"); got != "GARMIN_TEST_CLIENT" {
		t.Fatalf("client_id = %q, want the client id that issued the chain", got)
	}
	if got := gotForm.Get("refresh_token"); got != "original-refresh-fixture" {
		t.Fatalf("refresh_token = %q", got)
	}
	if len(gotForm) != 3 {
		t.Fatalf("refresh body carried %d fields (%v), want exactly grant_type, client_id, refresh_token", len(gotForm), gotForm)
	}

	// Garmin rotates the refresh token on every use; the rotated one must win.
	if next.RefreshToken != "rotated-refresh-fixture" {
		t.Fatalf("rotated refresh token not adopted: %q", next.RefreshToken)
	}
	if next.RefreshExpiry.IsZero() {
		t.Fatal("refresh_token_expires_in was not recorded")
	}
}

// A response that omits a new refresh token leaves the stored one in place,
// rather than blanking the chain.
func TestRefreshKeepsOldRefreshTokenWhenResponseOmitsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"` +
			fakeJWT(t, map[string]any{"client_id": "GARMIN_TEST_CLIENT", "exp": time.Now().Add(time.Hour).Unix()}) +
			`","expires_in":3600}`))
	}))
	defer server.Close()

	old := &garminTokens{AccessToken: "stale", RefreshToken: "keep-me", ClientID: "GARMIN_TEST_CLIENT", RefreshExpiry: time.Now().Add(72 * time.Hour)}
	next, err := garminRefreshTokens(context.Background(), garminHTTPClient(), endpointsFor(server), old)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if next.RefreshToken != "keep-me" {
		t.Fatalf("refresh token = %q, want the stored one preserved", next.RefreshToken)
	}
	if !next.RefreshExpiry.Equal(old.RefreshExpiry) {
		t.Fatalf("refresh expiry = %v, want the stored one preserved", next.RefreshExpiry)
	}
}

// Negative: a dead refresh token is reported as "sign in again", not as a
// transient failure to retry.
func TestRefreshRejectionIsTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()

	_, err := garminRefreshTokens(context.Background(), garminHTTPClient(), endpointsFor(server),
		&garminTokens{RefreshToken: "dead", ClientID: "GARMIN_TEST_CLIENT"})
	var rejected *garminRefreshRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("error = %v (%T), want garminRefreshRejectedError", err, err)
	}
	if !strings.Contains(err.Error(), "auth login") {
		t.Fatalf("refresh rejection does not tell the user to sign in again: %v", err)
	}
}

// Negative: a 403/429 at the token service is an edge block, reported without
// a retry.
func TestTokenServiceEdgeBlockIsNotRetried(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("blocked"))
	}))
	defer server.Close()

	_, err := garminExchangeServiceTicket(context.Background(), garminHTTPClient(), endpointsFor(server), "ST-fixture", "http://127.0.0.1:1/callback")
	var blocked *garminEdgeBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v (%T), want garminEdgeBlockedError", err, err)
	}
	if calls != 1 {
		t.Fatalf("token service called %d times; a single-use ticket must be exchanged once", calls)
	}
}

// The exchange, unlike refresh, does send the Basic header and the native app
// headers. This is the positive control for the refresh test above.
func TestExchangeSendsBasicAndNativeHeaders(t *testing.T) {
	var gotAuth, gotUA string
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		gotForm, _ = url.ParseQuery(string(body))
		_, _ = w.Write([]byte(`{"access_token":"` +
			fakeJWT(t, map[string]any{"client_id": garminDIClientID, "exp": time.Now().Add(time.Hour).Unix()}) +
			`","refresh_token":"r","expires_in":81744,"refresh_token_expires_in":2591999}`))
	}))
	defer server.Close()

	tokens, err := garminExchangeServiceTicket(context.Background(), garminHTTPClient(), endpointsFor(server), "ST-fixture", "http://127.0.0.1:1/callback?state=abc")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("exchange Authorization = %q, want a Basic header", gotAuth)
	}
	if gotUA != "GCM-Android-5.23" {
		t.Fatalf("exchange User-Agent = %q, want the native app UA", gotUA)
	}
	if got := gotForm.Get("grant_type"); got != garminDIGrantType {
		t.Fatalf("grant_type = %q, want the service-ticket grant", got)
	}
	if got := gotForm.Get("service_url"); got != "http://127.0.0.1:1/callback?state=abc" {
		t.Fatalf("service_url = %q, want the exact callback URL used at SSO", got)
	}
	if tokens.ClientID != garminDIClientID {
		t.Fatalf("client id = %q, want it read back from the token claims", tokens.ClientID)
	}
}

// Negative: a token response with no access_token or a non-positive
// expires_in is refused rather than stored.
func TestTokenResponseValidation(t *testing.T) {
	for name, body := range map[string]string{
		"no access token":   `{"refresh_token":"r","expires_in":10}`,
		"zero expires_in":   `{"access_token":"a","refresh_token":"r","expires_in":0}`,
		"malformed payload": `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			if _, err := garminExchangeServiceTicket(context.Background(), garminHTTPClient(), endpointsFor(server), "ST", "cb"); err == nil {
				t.Fatal("accepted an unusable token response")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Functional + negative: identity assertion
// ---------------------------------------------------------------------------

func identityServer(t *testing.T, email string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case garminPersonalInformationPath:
			_, _ = w.Write([]byte(`{"userInfo":{"email":"` + email + `","locale":"en-US"}}`))
		case garminSocialProfilePath:
			_, _ = w.Write([]byte(`{"profileId":9876543,"displayName":"placeholder-display-name"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestAssertEmailAcceptsCaseInsensitiveMatch(t *testing.T) {
	server := identityServer(t, "Household.Member@example.test")
	defer server.Close()

	got, err := garminAssertEmail(context.Background(), garminHTTPClient(), endpointsFor(server), "token-fixture", "household.member@EXAMPLE.test")
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	if got != "Household.Member@example.test" {
		t.Fatalf("asserted email = %q, want the address Garmin reported", got)
	}
}

// Negative: the signed-in account is somebody else. The caller must be able to
// name that account and must not be handed a success.
func TestAssertEmailRejectsAnotherAccount(t *testing.T) {
	server := identityServer(t, "someone.else@example.test")
	defer server.Close()

	got, err := garminAssertEmail(context.Background(), garminHTTPClient(), endpointsFor(server), "token-fixture", "intended@example.test")
	if !errors.Is(err, errGarminIdentityMismatch) {
		t.Fatalf("error = %v, want errGarminIdentityMismatch", err)
	}
	if got != "someone.else@example.test" {
		t.Fatalf("mismatch did not report the signed-in account, got %q", got)
	}
}

// Negative: an identity response with no email is a refusal, not a silent pass.
func TestAssertEmailRefusesResponseWithoutEmail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"userInfo":{"locale":"en-US"}}`))
	}))
	defer server.Close()

	if _, err := garminAssertEmail(context.Background(), garminHTTPClient(), endpointsFor(server), "token-fixture", "intended@example.test"); err == nil {
		t.Fatal("an identity response with no email was accepted")
	}
}

// ---------------------------------------------------------------------------
// Functional + negative: JWT claim reading
// ---------------------------------------------------------------------------

func TestJWTClaimsAndExpiry(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	token := fakeJWT(t, map[string]any{
		"client_id":   "GARMIN_TEST_CLIENT",
		"garmin_guid": "11111111-2222-3333-4444-555555555555",
		"exp":         exp.Unix(),
	})
	if got := garminJWTString(token, "garmin_guid"); got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("garmin_guid = %q", got)
	}
	if got := garminJWTString(token, "client_id"); got != "GARMIN_TEST_CLIENT" {
		t.Fatalf("client_id = %q", got)
	}
	if got := garminJWTExpiry(token); !got.Equal(exp) {
		t.Fatalf("exp = %v, want %v", got, exp)
	}
}

// Negative: a token whose header claims `alg: none` must yield no claims, so
// an unsigned payload cannot steer the client id, the account id or the
// expiry this CLI records.
func TestJWTAlgNoneIsRefused(t *testing.T) {
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token := enc(map[string]any{"alg": "none"}) + "." +
		enc(map[string]any{"garmin_guid": "attacker", "client_id": "attacker", "exp": time.Now().Add(time.Hour).Unix()}) + "."
	if got := garminJWTString(token, "garmin_guid"); got != "" {
		t.Fatalf("alg=none token yielded garmin_guid %q", got)
	}
	if got := garminJWTExpiry(token); !got.IsZero() {
		t.Fatalf("alg=none token yielded expiry %v", got)
	}
	// Garbage is equally inert.
	if got := garminJWTString("not-a-jwt", "garmin_guid"); got != "" {
		t.Fatalf("non-JWT yielded %q", got)
	}
}

func TestNeedsRefreshUsesTheSkew(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	inTenMinutes := fakeJWT(t, map[string]any{"exp": now.Add(10 * time.Minute).Unix()})
	inADay := fakeJWT(t, map[string]any{"exp": now.Add(24 * time.Hour).Unix()})

	if !garminNeedsRefresh(inTenMinutes, time.Time{}, now) {
		t.Fatal("a token expiring inside the 15-minute skew was not marked for refresh")
	}
	if garminNeedsRefresh(inADay, time.Time{}, now) {
		t.Fatal("a token good for a day was marked for refresh")
	}
	// No readable claim: the stored expiry decides.
	if garminNeedsRefresh("opaque", now.Add(24*time.Hour), now) {
		t.Fatal("stored expiry a day out was marked for refresh")
	}
	// No evidence at all: refresh, because one call is cheaper than a run of 401s.
	if !garminNeedsRefresh("opaque", time.Time{}, now) {
		t.Fatal("a token with no expiry evidence was not marked for refresh")
	}
}

// ---------------------------------------------------------------------------
// Functional: lock contention
// ---------------------------------------------------------------------------

// Two callers must not be inside the refresh-and-write window at once: Garmin
// rotates the refresh token on use, so the loser would persist a token Garmin
// has already retired.
func TestAuthLockSerializesConcurrentHolders(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")

	var (
		mu      sync.Mutex
		inside  int
		maxSeen int
		wg      sync.WaitGroup
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := garminWithAuthLock(configPath, func() error {
				mu.Lock()
				inside++
				if inside > maxSeen {
					maxSeen = inside
				}
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("lock: %v", err)
			}
		}()
	}
	wg.Wait()

	if maxSeen != 1 {
		t.Fatalf("%d holders were inside the lock at once, want 1", maxSeen)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), garminAuthLockName+".lock")); err != nil {
		t.Fatalf("lock file not created beside the config: %v", err)
	}
}

// An error from the guarded function propagates and still releases the lock.
func TestAuthLockReleasesOnError(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	sentinel := errors.New("guarded failure")
	if err := garminWithAuthLock(configPath, func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the guarded error", err)
	}
	done := make(chan struct{})
	go func() {
		_ = garminWithAuthLock(configPath, func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not released after a guarded failure")
	}
}

// ---------------------------------------------------------------------------
// Functional + negative: timeout message
// ---------------------------------------------------------------------------

// The timeout text has to say Garmin may have signed the user in regardless:
// a callback landing after the deadline shows a browser error page while the
// Garmin session is live, and the next attempt then silently signs in the
// wrong account.
func TestLoginTimeoutMessageWarnsAboutTheLiveSession(t *testing.T) {
	err := garminLoginTimeoutError(10*time.Minute, 2)
	msg := err.Error()
	for _, want := range []string{"10m0s", "2 callback(s) rejected", "Garmin may have signed you in anyway", "sign out in the browser"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("timeout message missing %q:\n%s", want, msg)
		}
	}
}

func TestLoginRequiresEmail(t *testing.T) {
	root := RootCmd()
	root.SetArgs([]string{"auth", "login"})
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.Execute()
	if err == nil {
		t.Fatal("auth login ran without --email")
	}
	if !strings.Contains(err.Error(), "--email is required") {
		t.Fatalf("error = %v, want the --email requirement", err)
	}
}

// The login default is 10 minutes and the listener is documented as staying
// open for all of it.
func TestLoginTimeoutDefaultIsTenMinutes(t *testing.T) {
	if garminLoginTimeoutDefault != 10*time.Minute {
		t.Fatalf("default login timeout = %s, want 10m", garminLoginTimeoutDefault)
	}
	root := RootCmd()
	cmd, _, err := root.Find([]string{"auth", "login"})
	if err != nil {
		t.Fatalf("find auth login: %v", err)
	}
	flag := cmd.Flags().Lookup("timeout")
	if flag == nil {
		t.Fatal("auth login has no --timeout flag")
	}
	if flag.DefValue != "10m0s" {
		t.Fatalf("--timeout default = %q, want 10m0s", flag.DefValue)
	}
	if cmd.Flags().Lookup("no-logout") == nil {
		t.Fatal("auth login has no --no-logout flag")
	}
}

// ---------------------------------------------------------------------------
// Functional: command wiring
// ---------------------------------------------------------------------------

// The hand-authored commands replace the generated ones rather than sitting
// beside them, and `set-token` stays wired as a hidden escape hatch.
func TestAuthCommandSetIsReplacedNotDuplicated(t *testing.T) {
	root := RootCmd()
	auth, _, err := root.Find([]string{"auth"})
	if err != nil {
		t.Fatalf("find auth: %v", err)
	}
	counts := map[string]int{}
	for _, child := range auth.Commands() {
		counts[child.Name()]++
	}
	for _, name := range []string{"login", "status", "logout", "setup", "set-token"} {
		if counts[name] != 1 {
			t.Fatalf("auth %s appears %d times, want exactly 1", name, counts[name])
		}
	}
	setToken, _, err := root.Find([]string{"auth", "set-token"})
	if err != nil {
		t.Fatalf("find auth set-token: %v", err)
	}
	if !setToken.Hidden {
		t.Fatal("auth set-token should be hidden; auth login is the supported path")
	}
	if setToken.RunE == nil {
		t.Fatal("auth set-token must stay runnable as an escape hatch")
	}
	status, _, err := root.Find([]string{"auth", "status"})
	if err != nil {
		t.Fatalf("find auth status: %v", err)
	}
	if status.Flags().Lookup("verify") == nil {
		t.Fatal("auth status has no --verify flag")
	}
}

// ---------------------------------------------------------------------------
// Functional + negative: identity sidecar and spike-file migration
// ---------------------------------------------------------------------------

func TestIdentitySidecarRoundTripsAndIsPrivate(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	want := &garminIdentity{
		Email:         "placeholder@example.test",
		GarminGUID:    "11111111-2222-3333-4444-555555555555",
		ProfileID:     "9876543",
		Domain:        garminDomainGlobal,
		ClientID:      "GARMIN_TEST_CLIENT",
		RefreshExpiry: time.Now().Add(720 * time.Hour).UTC().Truncate(time.Second),
		AssertedAt:    time.Now().UTC().Truncate(time.Second),
	}
	if err := garminSaveIdentity(configPath, want); err != nil {
		t.Fatalf("save identity: %v", err)
	}
	info, err := os.Stat(garminIdentityPath(configPath))
	if err != nil {
		t.Fatalf("stat identity: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("identity file mode = %o, want 600", perm)
	}
	got, err := garminLoadIdentity(configPath)
	if err != nil {
		t.Fatalf("load identity: %v", err)
	}
	if got.Email != want.Email || got.GarminGUID != want.GarminGUID || got.ProfileID != want.ProfileID {
		t.Fatalf("identity round trip mismatch: %+v", got)
	}

	if err := garminRemoveIdentity(configPath); err != nil {
		t.Fatalf("remove identity: %v", err)
	}
	if _, err := os.Stat(garminIdentityPath(configPath)); !os.IsNotExist(err) {
		t.Fatalf("identity file survived removal: %v", err)
	}
	// Removing an absent file is not an error: logout must be idempotent.
	if err := garminRemoveIdentity(configPath); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

// The N131.2 spike wrote a flat token file at this CLI's own config path, with
// a quoted RFC3339 token_expiry the generated loader cannot parse. Migration
// moves the secrets into the credentials file, records the account in the
// sidecar, and leaves a config.toml the generated loader accepts silently.
func TestSpikeTokenFileMigratesOnce(t *testing.T) {
	// GARMIN_HOME moves config, data, state and cache under one temp root, so
	// SaveTokens writes this test's credentials file and never the operator's.
	home := t.TempDir()
	t.Setenv("GARMIN_HOME", home)
	configPath := filepath.Join(home, "config", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("make config dir: %v", err)
	}
	access := fakeJWT(t, map[string]any{
		"client_id":   "GARMIN_TEST_CLIENT",
		"garmin_guid": "11111111-2222-3333-4444-555555555555",
		"exp":         time.Now().Add(20 * time.Hour).Unix(),
	})
	spike := "" +
		"di_client_id = \"GARMIN_TEST_CLIENT\"\n" +
		"di_refresh_token = \"refresh-fixture\"\n" +
		"di_token = \"" + access + "\"\n" +
		"domain = \"garmin.com\"\n" +
		"email = \"placeholder@example.test\"\n" +
		"refresh_expiry = \"2026-10-07T21:38:45.446367-07:00\"\n" +
		"token_expiry = \"2026-09-08T16:26:46.446367-07:00\"\n"
	if err := os.WriteFile(configPath, []byte(spike), 0o600); err != nil {
		t.Fatalf("write spike file: %v", err)
	}

	var notice strings.Builder
	migrated, err := garminMigrateSpikeTokenFile(configPath, "", &notice)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !migrated {
		t.Fatal("a spike token file was not recognised")
	}
	if !strings.Contains(notice.String(), "Migrated") {
		t.Fatalf("migration printed no clear message: %q", notice.String())
	}

	// config.toml now parses under the generated loader with no warning path.
	rewritten, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read rewritten config: %v", err)
	}
	if strings.Contains(string(rewritten), "di_token") || strings.Contains(string(rewritten), "di_refresh_token") {
		t.Fatalf("the spike token keys survived in config.toml:\n%s", rewritten)
	}

	// Secrets landed in the credentials file the generated client reads.
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load migrated config: %v", err)
	}
	if cfg.AccessToken != access {
		t.Fatal("the access token did not reach the credentials file")
	}
	if cfg.RefreshToken != "refresh-fixture" {
		t.Fatalf("refresh token = %q", cfg.RefreshToken)
	}
	if cfg.AuthHeader() != "Bearer "+access {
		t.Fatal("the migrated credential does not produce a bearer header")
	}

	// Identity landed in the sidecar.
	id, err := garminLoadIdentity(configPath)
	if err != nil {
		t.Fatalf("load identity: %v", err)
	}
	if id.Email != "placeholder@example.test" {
		t.Fatalf("identity email = %q", id.Email)
	}
	if id.GarminGUID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("identity garmin_guid = %q, want the JWT claim", id.GarminGUID)
	}
	if id.RefreshExpiry.IsZero() {
		t.Fatal("the spike's refresh expiry was not carried over")
	}

	// Idempotent: a second pass finds nothing to do.
	again, err := garminMigrateSpikeTokenFile(configPath, "", io.Discard)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if again {
		t.Fatal("migration ran twice on the same file")
	}
}

// Negative: an ordinary press config is not mistaken for a spike token file.
func TestMigrationIgnoresAnOrdinaryConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GARMIN_HOME", home)
	configPath := filepath.Join(home, "config", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("make config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("base_url = 'https://connectapi.garmin.com'\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	migrated, err := garminMigrateSpikeTokenFile(configPath, "", io.Discard)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if migrated {
		t.Fatal("an ordinary config was treated as a spike token file")
	}
	data, _ := os.ReadFile(configPath)
	if !strings.Contains(string(data), "connectapi.garmin.com") {
		t.Fatalf("an ordinary config was rewritten:\n%s", data)
	}
}

// ---------------------------------------------------------------------------
// Functional + negative: pre-call refresh hook
// ---------------------------------------------------------------------------

func TestEnsureFreshTokenSkipsUnderVerifyAndEnvCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the refresh hook dialled out when it should not have")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	expired := fakeJWT(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})

	t.Run("verify env", func(t *testing.T) {
		t.Setenv("PRINTING_PRESS_VERIFY", "1")
		cfg := &config.Config{Path: filepath.Join(t.TempDir(), "config.toml"), AccessToken: expired, RefreshToken: "r", ClientID: "c"}
		if err := garminEnsureFreshToken(cfg, io.Discard, time.Now()); err != nil {
			t.Fatalf("hook under verify: %v", err)
		}
	})

	t.Run("env credential", func(t *testing.T) {
		cfg := &config.Config{Path: filepath.Join(t.TempDir(), "config.toml"), AccessToken: expired, RefreshToken: "r", ClientID: "c", AuthSource: "env:GARMIN_TOKEN"}
		if err := garminEnsureFreshToken(cfg, io.Discard, time.Now()); err != nil {
			t.Fatalf("hook with an env credential: %v", err)
		}
	})

	t.Run("no refresh token", func(t *testing.T) {
		cfg := &config.Config{Path: filepath.Join(t.TempDir(), "config.toml"), AccessToken: expired}
		if err := garminEnsureFreshToken(cfg, io.Discard, time.Now()); err != nil {
			t.Fatalf("hook with no refresh token: %v", err)
		}
	})

	t.Run("token still fresh", func(t *testing.T) {
		fresh := fakeJWT(t, map[string]any{"exp": time.Now().Add(20 * time.Hour).Unix()})
		cfg := &config.Config{Path: filepath.Join(t.TempDir(), "config.toml"), AccessToken: fresh, RefreshToken: "r", ClientID: "c"}
		if err := garminEnsureFreshToken(cfg, io.Discard, time.Now()); err != nil {
			t.Fatalf("hook with a fresh token: %v", err)
		}
	})
}

// A refresh that fails while the access token is still usable warns and lets
// the command run; only a dead token is fatal.
func TestEnsureFreshTokenDowngradesToAWarningWhileTheTokenStillWorks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GARMIN_HOME", home)
	configPath := filepath.Join(home, "config", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("make config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("base_url = 'https://connectapi.garmin.com'\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// Inside the 15-minute skew but not yet expired.
	nearlyExpired := fakeJWT(t, map[string]any{"exp": time.Now().Add(5 * time.Minute).Unix()})
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AccessToken = nearlyExpired
	cfg.RefreshToken = "refresh-fixture"
	cfg.ClientID = "GARMIN_TEST_CLIENT"

	// Point the token service at a closed loopback port: the refresh cannot
	// succeed and, critically, never reaches Garmin.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	deadURL := dead.URL + "/di-oauth2-service/oauth/token"
	dead.Close()
	t.Setenv("GARMIN_TOKEN_URL", deadURL)

	var warnings strings.Builder
	if err := garminEnsureFreshToken(cfg, &warnings, time.Now()); err != nil {
		t.Fatalf("a failed refresh with a still-valid token must not fail the command: %v", err)
	}
	if !strings.Contains(warnings.String(), "could not refresh") {
		t.Fatalf("no warning was emitted: %q", warnings.String())
	}
}

// ---------------------------------------------------------------------------
// Performance
// ---------------------------------------------------------------------------

// Every command construction runs the refresh hook, and every hook run reads
// the token claims. The no-op path (token still fresh) must stay far below
// human-perceptible cost: target under 50ms for 10,000 evaluations.
func TestNeedsRefreshEvaluationIsCheap(t *testing.T) {
	token := fakeJWT(t, map[string]any{"exp": time.Now().Add(20 * time.Hour).Unix()})
	now := time.Now()
	start := time.Now()
	for i := 0; i < 10000; i++ {
		if garminNeedsRefresh(token, time.Time{}, now) {
			t.Fatal("a fresh token was marked for refresh")
		}
	}
	elapsed := time.Since(start)
	if elapsed > 50*time.Millisecond {
		t.Fatalf("10,000 refresh evaluations took %s, target under 50ms", elapsed)
	}
	t.Logf("10,000 refresh evaluations in %s", elapsed)
}

// The lock is taken on every refresh-and-write. An uncontended acquisition
// must not add measurable latency: target under 100ms for 200 cycles.
func TestUncontendedLockIsCheap(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	start := time.Now()
	for i := 0; i < 200; i++ {
		if err := garminWithAuthLock(configPath, func() error { return nil }); err != nil {
			t.Fatalf("lock cycle %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("200 uncontended lock cycles took %s, target under 100ms", elapsed)
	}
	t.Logf("200 uncontended lock cycles in %s", elapsed)
}

// ---------------------------------------------------------------------------
// N131.4g audit fixes
// ---------------------------------------------------------------------------

// Negative (TB-F1): every call built on ConnectAPI carries a freshly minted
// Garmin bearer, so a persisted base_url or an ambient GARMIN_BASE_URL must not
// be able to redirect the identity check at an arbitrary host.
func TestBaseURLOverrideIsLoopbackOnly(t *testing.T) {
	defaultAPI := garminNewEndpoints(garminDomainGlobal).ConnectAPI

	t.Run("loopback accepted", func(t *testing.T) {
		cfg := &config.Config{BaseURL: "http://127.0.0.1:8123"}
		if got := garminEndpointsFor(cfg, garminDomainGlobal).ConnectAPI; got != "http://127.0.0.1:8123" {
			t.Fatalf("loopback override not honoured: %q", got)
		}
	})
	t.Run("the domain default is not an override", func(t *testing.T) {
		cfg := &config.Config{BaseURL: defaultAPI + "/"}
		if got := garminEndpointsFor(cfg, garminDomainGlobal).ConnectAPI; got != defaultAPI {
			t.Fatalf("ConnectAPI = %q, want the domain default %q", got, defaultAPI)
		}
	})
	for name, raw := range map[string]string{
		"remote host":      "https://collector.example.test",
		"garmin-lookalike": "https://connectapi.garmin.com.example.test",
		"not a url":        "::::",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{BaseURL: raw}
			if got := garminEndpointsFor(cfg, garminDomainGlobal).ConnectAPI; got != defaultAPI {
				t.Fatalf("base_url %q redirected the account check to %q; only loopback may", raw, got)
			}
		})
	}
	t.Run("a nil config leaves the default", func(t *testing.T) {
		if got := garminEndpointsFor(nil, garminDomainGlobal).ConnectAPI; got != defaultAPI {
			t.Fatalf("ConnectAPI = %q, want %q", got, defaultAPI)
		}
	})
}

// TB-F6: only `auth login` and `auth status --verify` assert which account a
// chain belongs to, so an env-supplied credential must not inherit the account
// the sidecar recorded for this home.
func TestCredentialAssertionTracksTheChainInUse(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *config.Config
		verified bool
		want     bool
	}{
		{"stored chain", &config.Config{AuthSource: "credentials file"}, false, true},
		{"env credential", &config.Config{AuthSource: "env:GARMIN_ACCESS_TOKEN"}, false, false},
		{"env credential just verified", &config.Config{AuthSource: "env:GARMIN_ACCESS_TOKEN"}, true, true},
		{"no config", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := garminCredentialIsAsserted(tc.cfg, tc.verified); got != tc.want {
				t.Fatalf("garminCredentialIsAsserted = %v, want %v", got, tc.want)
			}
		})
	}
	note := garminUnassertedCredentialNote(&config.Config{AuthSource: "env:GARMIN_ACCESS_TOKEN"})
	if !strings.Contains(note, "env:GARMIN_ACCESS_TOKEN") || !strings.Contains(note, "auth status --verify") {
		t.Fatalf("the note names neither the source nor the fix: %q", note)
	}
	if got := garminUnassertedCredentialNote(nil); !strings.Contains(got, "external credential") {
		t.Fatalf("a nil config produced %q", got)
	}
}

// Negative (TB-F4): the migration rewrites config.toml before it can load it,
// so between those two writes the refresh chain exists only in memory. A
// failure there must put the original token file back rather than lose it.
func TestSpikeMigrationRestoresTheTokenFileWhenTheCredentialWriteFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GARMIN_HOME", home)
	// Point the data kind at a path whose parent is a regular file, so the
	// credentials write fails after config.toml has already been rewritten.
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv("GARMIN_DATA_DIR", filepath.Join(blocker, "data"))

	configPath := filepath.Join(home, "config", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("make config dir: %v", err)
	}
	access := fakeJWT(t, map[string]any{
		"client_id":   "GARMIN_TEST_CLIENT",
		"garmin_guid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"exp":         time.Now().Add(20 * time.Hour).Unix(),
	})
	spike := "" +
		"di_client_id = \"GARMIN_TEST_CLIENT\"\n" +
		"di_refresh_token = \"refresh-fixture\"\n" +
		"di_token = \"" + access + "\"\n" +
		"domain = \"garmin.com\"\n" +
		"email = \"placeholder@example.test\"\n"
	if err := os.WriteFile(configPath, []byte(spike), 0o600); err != nil {
		t.Fatalf("write spike file: %v", err)
	}

	migrated, err := garminMigrateSpikeTokenFile(configPath, "", io.Discard)
	if err == nil {
		t.Fatal("the migration reported success although the credentials could not be written")
	}
	if migrated {
		t.Fatal("a failed migration reported that it migrated")
	}
	after, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("the spike token file is gone after a failed migration: %v", readErr)
	}
	if string(after) != spike {
		t.Fatalf("the spike token file was not restored byte-for-byte:\n%s", after)
	}
	// A retry once the obstruction is gone still works, which is the point of
	// keeping the file.
	t.Setenv("GARMIN_DATA_DIR", filepath.Join(home, "data"))
	again, err := garminMigrateSpikeTokenFile(configPath, "", io.Discard)
	if err != nil || !again {
		t.Fatalf("retry after restore: migrated=%v err=%v", again, err)
	}
}

// Negative (TB-F10): the identity sidecar holds an account email, so a
// group-readable file is refused rather than read.
func TestIdentitySidecarRefusesLoosePermissions(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	if err := garminSaveIdentity(configPath, &garminIdentity{Email: "placeholder@example.test"}); err != nil {
		t.Fatalf("save identity: %v", err)
	}
	if _, err := garminLoadIdentity(configPath); err != nil {
		t.Fatalf("a 0600 sidecar was refused: %v", err)
	}
	if err := os.Chmod(garminIdentityPath(configPath), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := garminLoadIdentity(configPath); err == nil {
		t.Fatal("a group/world-readable sidecar was read")
	}
}

// TB-F6, contract level: `auth status --json` must not report the sidecar
// email as this home's account when the credential in use came from the
// environment, because nothing asserted that chain. The predicate is covered
// above; this pins the JSON envelope an agent reads.
func TestAuthStatusJSONSeparatesAssertedEmailFromAnEnvCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GARMIN_HOME", home)
	configDir := filepath.Join(home, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("make config dir: %v", err)
	}
	configPath := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("base_url = 'https://connectapi.garmin.com'\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := garminSaveIdentity(configPath, &garminIdentity{
		Email:     "placeholder@example.test",
		ProfileID: "9999999",
	}); err != nil {
		t.Fatalf("save identity: %v", err)
	}

	run := func(t *testing.T) map[string]any {
		t.Helper()
		flags := &rootFlags{asJSON: true}
		cmd := newGarminAuthStatusCmd(flags)
		var out, errBuf bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errBuf)
		cmd.SetArgs(nil)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("auth status --json: %v (stderr %q)", err, errBuf.String())
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		return got
	}

	t.Run("env credential does not inherit the asserted account", func(t *testing.T) {
		t.Setenv("GARMIN_ACCESS_TOKEN", "synthetic-not-a-real-token")
		got := run(t)
		if _, ok := got["email"]; ok {
			t.Fatalf("an env credential was reported as this home's account: %v", got["email"])
		}
		if got["email_last_asserted"] != "placeholder@example.test" {
			t.Fatalf("email_last_asserted = %v", got["email_last_asserted"])
		}
		if got["email_matches_credential_in_use"] != false {
			t.Fatalf("email_matches_credential_in_use = %v, want false", got["email_matches_credential_in_use"])
		}
		note, _ := got["credential_note"].(string)
		if !strings.Contains(note, "env:") || !strings.Contains(note, "auth status --verify") {
			t.Fatalf("credential_note = %q", note)
		}
	})

	t.Run("a stored chain still reports its account", func(t *testing.T) {
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		if err := cfg.SaveTokens("GARMIN_TEST_CLIENT", "", "synthetic-not-a-real-token", "synthetic-refresh", time.Now().Add(20*time.Hour)); err != nil {
			t.Fatalf("save tokens: %v", err)
		}
		got := run(t)
		if got["email"] != "placeholder@example.test" {
			t.Fatalf("email = %v, want the asserted account", got["email"])
		}
		if _, ok := got["email_last_asserted"]; ok {
			t.Fatal("a stored chain was labelled unasserted")
		}
		if _, ok := got["credential_note"]; ok {
			t.Fatal("a stored chain carried an unasserted-credential note")
		}
	})
}
