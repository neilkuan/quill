package teams

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newMockJWKSServer starts an httptest server that exposes both an OpenID
// config endpoint at /openid-configuration and a JWKS endpoint at /keys.
// jwksHandler receives a pointer to a per-request counter so tests can assert
// how many times the JWKS was refreshed.
func newMockJWKSServer(t *testing.T, keys []jwksKey) (configURL string, jwksCalls *int32, cleanup func()) {
	t.Helper()
	var calls int32
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)

	mux.HandleFunc("/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(openIDConfig{JWKSURI: ts.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(jwksResponse{Keys: keys})
	})

	return ts.URL + "/openid-configuration", &calls, ts.Close
}

func publicKeyAsJWK(pub *rsa.PublicKey, kid string) jwksKey {
	return jwksKey{
		Kid: kid,
		Kty: "RSA",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func mustGenRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	// 1024 bits keeps test generation fast; production tokens use 2048+.
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return priv
}

func TestJwkToRSA_ValidKey(t *testing.T) {
	priv := mustGenRSA(t)
	jwk := publicKeyAsJWK(&priv.PublicKey, "kid-1")

	pub, err := jwkToRSA(jwk)
	if err != nil {
		t.Fatalf("jwkToRSA: %v", err)
	}
	if pub.N.Cmp(priv.PublicKey.N) != 0 {
		t.Error("modulus mismatch")
	}
	if pub.E != priv.PublicKey.E {
		t.Errorf("exponent mismatch: got %d, want %d", pub.E, priv.PublicKey.E)
	}
}

func TestJwkToRSA_InvalidBase64(t *testing.T) {
	cases := []struct {
		name string
		jwk  jwksKey
	}{
		{"bad n", jwksKey{Kid: "x", Kty: "RSA", N: "!not-base64!", E: "AQAB"}},
		{"bad e", jwksKey{Kid: "x", Kty: "RSA", N: "AQAB", E: "!not-base64!"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := jwkToRSA(tc.jwk); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestJWKSProvider_FirstFetchAndCache(t *testing.T) {
	priv := mustGenRSA(t)
	configURL, jwksCalls, cleanup := newMockJWKSServer(t, []jwksKey{publicKeyAsJWK(&priv.PublicKey, "kid-1")})
	defer cleanup()

	p := NewJWKSProvider(configURL)

	if _, err := p.GetKey("kid-1"); err != nil {
		t.Fatalf("first GetKey: %v", err)
	}
	if got := atomic.LoadInt32(jwksCalls); got != 1 {
		t.Errorf("expected 1 JWKS fetch, got %d", got)
	}

	// Second call hits cache — no additional fetch.
	if _, err := p.GetKey("kid-1"); err != nil {
		t.Fatalf("second GetKey: %v", err)
	}
	if got := atomic.LoadInt32(jwksCalls); got != 1 {
		t.Errorf("cache miss: expected 1 total fetch, got %d", got)
	}
}

func TestJWKSProvider_RefreshOnStale(t *testing.T) {
	priv := mustGenRSA(t)
	configURL, jwksCalls, cleanup := newMockJWKSServer(t, []jwksKey{publicKeyAsJWK(&priv.PublicKey, "kid-1")})
	defer cleanup()

	p := NewJWKSProvider(configURL)
	if _, err := p.GetKey("kid-1"); err != nil {
		t.Fatalf("first GetKey: %v", err)
	}

	// Simulate stale cache by rewinding the fetched timestamp.
	p.mu.Lock()
	p.fetched = time.Now().Add(-jwksCacheTTL - time.Minute)
	p.mu.Unlock()

	if _, err := p.GetKey("kid-1"); err != nil {
		t.Fatalf("stale GetKey: %v", err)
	}
	if got := atomic.LoadInt32(jwksCalls); got != 2 {
		t.Errorf("expected 2 fetches after stale, got %d", got)
	}
}

func TestJWKSProvider_UnknownKid(t *testing.T) {
	priv := mustGenRSA(t)
	configURL, _, cleanup := newMockJWKSServer(t, []jwksKey{publicKeyAsJWK(&priv.PublicKey, "kid-1")})
	defer cleanup()

	p := NewJWKSProvider(configURL)
	if _, err := p.GetKey("unknown-kid"); err == nil {
		t.Error("expected error for unknown kid")
	}
}

func TestJWKSProvider_MissingJWKSURI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := NewJWKSProvider(ts.URL + "/openid-configuration")
	if _, err := p.GetKey("any"); err == nil {
		t.Error("expected error for missing jwks_uri")
	}
}

func TestJWKSProvider_OpenIDConfig5xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	p := NewJWKSProvider(ts.URL)
	if _, err := p.GetKey("any"); err == nil {
		t.Error("expected error when openid config returns 500")
	}
}

func TestJWKSProvider_SkipsNonRSAKeys(t *testing.T) {
	priv := mustGenRSA(t)
	rsaKey := publicKeyAsJWK(&priv.PublicKey, "kid-1")
	ecKey := jwksKey{Kid: "ec-key", Kty: "EC", N: "ignored", E: "ignored"}

	configURL, _, cleanup := newMockJWKSServer(t, []jwksKey{ecKey, rsaKey})
	defer cleanup()

	p := NewJWKSProvider(configURL)
	if _, err := p.GetKey("kid-1"); err != nil {
		t.Fatalf("GetKey RSA: %v", err)
	}
	if _, err := p.GetKey("ec-key"); err == nil {
		t.Error("expected EC key to be skipped and return error")
	}
}
