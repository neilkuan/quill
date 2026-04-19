package teams

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	botFrameworkOpenIDURL   = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	tenantOpenIDURLTemplate = "https://login.microsoftonline.com/%s/v2.0/.well-known/openid-configuration"
	jwksCacheTTL            = 24 * time.Hour
)

type jwksKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksResponse struct {
	Keys []jwksKey `json:"keys"`
}

type openIDConfig struct {
	JWKSURI string `json:"jwks_uri"`
}

// JWKSProvider fetches and caches RSA public keys from a JWKS endpoint
// discovered via OpenID configuration.
type JWKSProvider struct {
	configURL  string
	httpClient *http.Client

	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func NewJWKSProvider(configURL string) *JWKSProvider {
	return &JWKSProvider{
		configURL:  configURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		keys:       make(map[string]*rsa.PublicKey),
	}
}

// GetKey returns the RSA public key for the given kid, refreshing the cache
// if stale or if the kid is unknown.
func (p *JWKSProvider) GetKey(kid string) (*rsa.PublicKey, error) {
	p.mu.RLock()
	if k, ok := p.keys[kid]; ok && time.Since(p.fetched) < jwksCacheTTL {
		p.mu.RUnlock()
		return k, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.keys[kid]; ok && time.Since(p.fetched) < jwksCacheTTL {
		return k, nil
	}

	if err := p.refreshLocked(); err != nil {
		return nil, err
	}

	if k, ok := p.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("kid %q not found in JWKS at %s", kid, p.configURL)
}

func (p *JWKSProvider) refreshLocked() error {
	jwksURI, err := p.fetchJWKSURI()
	if err != nil {
		return fmt.Errorf("fetch openid config: %w", err)
	}

	keys, err := p.fetchJWKS(jwksURI)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}

	p.keys = keys
	p.fetched = time.Now()
	return nil
}

func (p *JWKSProvider) fetchJWKSURI() (string, error) {
	resp, err := p.httpClient.Get(p.configURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openid config returned %d", resp.StatusCode)
	}
	var cfg openIDConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return "", err
	}
	if cfg.JWKSURI == "" {
		return "", fmt.Errorf("openid config missing jwks_uri")
	}
	return cfg.JWKSURI, nil
}

func (p *JWKSProvider) fetchJWKS(jwksURI string) (map[string]*rsa.PublicKey, error) {
	resp, err := p.httpClient.Get(jwksURI)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks endpoint returned %d", resp.StatusCode)
	}
	var set jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}

	out := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := jwkToRSA(k)
		if err != nil {
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("jwks contained no usable RSA keys")
	}
	return out, nil
}

func jwkToRSA(k jwksKey) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, fmt.Errorf("exponent too large")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e.Int64()),
	}, nil
}
