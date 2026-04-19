package teams

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

const (
	defaultTokenURL    = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"
	botFrameworkScope  = "https://api.botframework.com/.default"
	tokenRefreshBuffer = 5 * time.Minute
)

type BotAuth struct {
	appID     string
	appSecret string
	tenantID  string
	tokenURL  string // overridable for testing

	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time

	// JWKS providers for inbound signature verification.
	// Exported fields allow tests to inject fakes.
	BotFrameworkJWKS *JWKSProvider
	TenantJWKS       *JWKSProvider
}

func NewBotAuth(appID, appSecret, tenantID string) *BotAuth {
	return &BotAuth{
		appID:            appID,
		appSecret:        appSecret,
		tenantID:         tenantID,
		tokenURL:         fmt.Sprintf(defaultTokenURL, tenantID),
		BotFrameworkJWKS: NewJWKSProvider(botFrameworkOpenIDURL),
		TenantJWKS:       NewJWKSProvider(fmt.Sprintf(tenantOpenIDURLTemplate, tenantID)),
	}
}

func (a *BotAuth) GetBotToken() (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()

	if a.token != "" && time.Now().Before(a.tokenExpiry) {
		return a.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.appID},
		"client_secret": {a.appSecret},
		"scope":         {botFrameworkScope},
	}

	resp, err := http.Post(a.tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}

	a.token = tokenResp.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn)*time.Second - tokenRefreshBuffer)

	return a.token, nil
}

func (a *BotAuth) ValidateInbound(r *http.Request) error {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return fmt.Errorf("missing Authorization header")
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return fmt.Errorf("invalid Authorization header format")
	}

	tokenString := parts[1]
	return a.validateJWT(tokenString)
}

// validateJWT verifies the token's RS256 signature against the Bot Framework
// or tenant JWKS, and checks that audience and issuer match this bot.
func (a *BotAuth) validateJWT(tokenString string) error {
	keyFunc := func(token *jwtv5.Token) (interface{}, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("missing kid in JWT header")
		}
		claims, ok := token.Claims.(jwtv5.MapClaims)
		if !ok {
			return nil, fmt.Errorf("unexpected claims type")
		}
		iss, _ := claims["iss"].(string)
		provider := a.providerForIssuer(iss)
		if provider == nil {
			return nil, fmt.Errorf("invalid issuer: %s", iss)
		}
		return provider.GetKey(kid)
	}

	if _, err := jwtv5.Parse(tokenString, keyFunc,
		jwtv5.WithValidMethods([]string{"RS256"}),
		jwtv5.WithAudience(a.appID),
		jwtv5.WithIssuedAt(),
	); err != nil {
		return fmt.Errorf("verify JWT: %w", err)
	}
	return nil
}

func (a *BotAuth) providerForIssuer(iss string) *JWKSProvider {
	switch iss {
	case "https://api.botframework.com":
		return a.BotFrameworkJWKS
	case "https://sts.windows.net/" + a.tenantID + "/",
		"https://login.microsoftonline.com/" + a.tenantID + "/v2.0":
		return a.TenantJWKS
	}
	return nil
}
