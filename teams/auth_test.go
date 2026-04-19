package teams

import (
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

func TestGetBotToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			t.Errorf("unexpected grant_type: %s", r.Form.Get("grant_type"))
		}
		if r.Form.Get("client_id") != "test-app-id" {
			t.Errorf("unexpected client_id: %s", r.Form.Get("client_id"))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "mock-bot-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer ts.Close()

	auth := &BotAuth{
		appID:     "test-app-id",
		appSecret: "test-secret",
		tenantID:  "test-tenant",
		tokenURL:  ts.URL,
	}

	token, err := auth.GetBotToken()
	if err != nil {
		t.Fatalf("GetBotToken: %v", err)
	}
	if token != "mock-bot-token" {
		t.Errorf("expected mock-bot-token, got %s", token)
	}

	// Second call should return cached token
	token2, err := auth.GetBotToken()
	if err != nil {
		t.Fatalf("GetBotToken (cached): %v", err)
	}
	if token2 != "mock-bot-token" {
		t.Errorf("expected cached token, got %s", token2)
	}
}

func TestValidateInbound_MissingHeader(t *testing.T) {
	auth := NewBotAuth("test-app-id", "test-secret", "test-tenant")
	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)

	err := auth.ValidateInbound(r)
	if err == nil {
		t.Error("expected error for missing Authorization header")
	}
}

func TestValidateInbound_InvalidFormat(t *testing.T) {
	auth := NewBotAuth("test-app-id", "test-secret", "test-tenant")
	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	r.Header.Set("Authorization", "InvalidFormat")

	err := auth.ValidateInbound(r)
	if err == nil {
		t.Error("expected error for invalid Authorization format")
	}
}

// signTestJWT returns a Bearer-ready RS256 JWT signed by priv.
func signTestJWT(t *testing.T, priv *rsa.PrivateKey, kid, iss, aud string) string {
	t.Helper()
	claims := jwtv5.MapClaims{
		"iss": iss,
		"aud": aud,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signed
}

// newAuthWithMockJWKS wires a BotAuth whose BotFrameworkJWKS serves the test
// public key, leaving TenantJWKS unreachable.
func newAuthWithMockJWKS(t *testing.T, priv *rsa.PrivateKey, kid, appID, tenantID string) (*BotAuth, func()) {
	t.Helper()
	configURL, _, cleanup := newMockJWKSServer(t, []jwksKey{publicKeyAsJWK(&priv.PublicKey, kid)})

	auth := &BotAuth{
		appID:            appID,
		tenantID:         tenantID,
		BotFrameworkJWKS: NewJWKSProvider(configURL),
		TenantJWKS:       NewJWKSProvider("http://127.0.0.1:1/unused"),
	}
	return auth, cleanup
}

func TestValidateInbound_ValidToken(t *testing.T) {
	priv := mustGenRSA(t)
	auth, cleanup := newAuthWithMockJWKS(t, priv, "kid-1", "test-app-id", "test-tenant")
	defer cleanup()

	token := signTestJWT(t, priv, "kid-1", "https://api.botframework.com", "test-app-id")

	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	if err := auth.ValidateInbound(r); err != nil {
		t.Fatalf("ValidateInbound: %v", err)
	}
}

func TestValidateInbound_InvalidIssuer(t *testing.T) {
	priv := mustGenRSA(t)
	auth, cleanup := newAuthWithMockJWKS(t, priv, "kid-1", "test-app-id", "test-tenant")
	defer cleanup()

	token := signTestJWT(t, priv, "kid-1", "https://evil.example.com", "test-app-id")

	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	if err := auth.ValidateInbound(r); err == nil {
		t.Error("expected error for invalid issuer")
	}
}

func TestValidateInbound_InvalidAudience(t *testing.T) {
	priv := mustGenRSA(t)
	auth, cleanup := newAuthWithMockJWKS(t, priv, "kid-1", "test-app-id", "test-tenant")
	defer cleanup()

	token := signTestJWT(t, priv, "kid-1", "https://api.botframework.com", "other-app")

	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	if err := auth.ValidateInbound(r); err == nil {
		t.Error("expected error for audience mismatch")
	}
}

func TestValidateInbound_TamperedSignature(t *testing.T) {
	priv := mustGenRSA(t)
	auth, cleanup := newAuthWithMockJWKS(t, priv, "kid-1", "test-app-id", "test-tenant")
	defer cleanup()

	token := signTestJWT(t, priv, "kid-1", "https://api.botframework.com", "test-app-id")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected header.payload.signature, got %d parts", len(parts))
	}
	// Flip 8 bytes in the middle of the signature segment to guarantee multiple
	// bits change regardless of base64url alignment.
	sig := []byte(parts[2])
	if len(sig) < 8 {
		t.Fatalf("signature segment too short: %d", len(sig))
	}
	mid := len(sig) / 2
	for i := 0; i < 8; i++ {
		if sig[mid+i] == 'A' {
			sig[mid+i] = 'B'
		} else {
			sig[mid+i] = 'A'
		}
	}
	tampered := parts[0] + "." + parts[1] + "." + string(sig)
	if tampered == token {
		t.Fatal("tampering produced identical token")
	}

	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	r.Header.Set("Authorization", "Bearer "+tampered)

	err := auth.ValidateInbound(r)
	if err == nil {
		t.Fatal("expected error for tampered signature")
	}
	// Confirm it was the signature check that rejected it, not some other stage.
	if !errors.Is(err, jwtv5.ErrTokenSignatureInvalid) {
		t.Errorf("expected ErrTokenSignatureInvalid, got %v", err)
	}
}

func TestValidateInbound_UnknownKid(t *testing.T) {
	priv := mustGenRSA(t)
	auth, cleanup := newAuthWithMockJWKS(t, priv, "kid-1", "test-app-id", "test-tenant")
	defer cleanup()

	token := signTestJWT(t, priv, "kid-unknown", "https://api.botframework.com", "test-app-id")

	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	if err := auth.ValidateInbound(r); err == nil {
		t.Error("expected error for unknown kid")
	}
}

// TestValidateInbound_NoneAlgRejected ensures tokens with alg=none (a classic
// JWT bypass) cannot pass validation.
func TestValidateInbound_NoneAlgRejected(t *testing.T) {
	priv := mustGenRSA(t)
	auth, cleanup := newAuthWithMockJWKS(t, priv, "kid-1", "test-app-id", "test-tenant")
	defer cleanup()

	claims := jwtv5.MapClaims{
		"iss": "https://api.botframework.com",
		"aud": "test-app-id",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodNone, claims)
	tok.Header["kid"] = "kid-1"
	unsigned, err := tok.SignedString(jwtv5.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none JWT: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	r.Header.Set("Authorization", "Bearer "+unsigned)

	err = auth.ValidateInbound(r)
	if err == nil {
		t.Fatal("expected error for alg=none token")
	}
	// Confirm WithValidMethods was the guard that rejected it. jwt/v5 wraps
	// method-mismatch failures with ErrTokenSignatureInvalid — so a bare Is
	// check here is strict enough to catch regressions where alg=none slips
	// past method validation.
	if !errors.Is(err, jwtv5.ErrTokenSignatureInvalid) {
		t.Errorf("expected ErrTokenSignatureInvalid from WithValidMethods, got %v", err)
	}
}

// TestValidateInbound_TenantIssuer confirms providerForIssuer routes tenant
// issuers to TenantJWKS (the non-default branch).
func TestValidateInbound_TenantIssuer(t *testing.T) {
	priv := mustGenRSA(t)

	// Stand up two distinct JWKS servers so we can tell which one served the key.
	tenantConfigURL, _, tenantCleanup := newMockJWKSServer(t, []jwksKey{publicKeyAsJWK(&priv.PublicKey, "kid-tenant")})
	defer tenantCleanup()

	auth := &BotAuth{
		appID:    "test-app-id",
		tenantID: "test-tenant",
		// Bot Framework JWKS should NOT be consulted for tenant issuers.
		BotFrameworkJWKS: NewJWKSProvider("http://127.0.0.1:1/unused"),
		TenantJWKS:       NewJWKSProvider(tenantConfigURL),
	}

	cases := []string{
		"https://sts.windows.net/test-tenant/",
		"https://login.microsoftonline.com/test-tenant/v2.0",
	}
	for _, iss := range cases {
		t.Run(iss, func(t *testing.T) {
			token := signTestJWT(t, priv, "kid-tenant", iss, "test-app-id")
			r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
			r.Header.Set("Authorization", "Bearer "+token)

			if err := auth.ValidateInbound(r); err != nil {
				t.Fatalf("ValidateInbound: %v", err)
			}
		})
	}
}

func TestValidateInbound_FutureIssuedAt(t *testing.T) {
	priv := mustGenRSA(t)
	auth, cleanup := newAuthWithMockJWKS(t, priv, "kid-1", "test-app-id", "test-tenant")
	defer cleanup()

	claims := jwtv5.MapClaims{
		"iss": "https://api.botframework.com",
		"aud": "test-app-id",
		"iat": time.Now().Add(1 * time.Hour).Unix(),
		"exp": time.Now().Add(2 * time.Hour).Unix(),
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodRS256, claims)
	tok.Header["kid"] = "kid-1"
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	if err := auth.ValidateInbound(r); err == nil {
		t.Error("expected error for future iat")
	}
}
