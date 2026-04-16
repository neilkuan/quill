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
}

func NewBotAuth(appID, appSecret, tenantID string) *BotAuth {
	return &BotAuth{
		appID:     appID,
		appSecret: appSecret,
		tenantID:  tenantID,
		tokenURL:  fmt.Sprintf(defaultTokenURL, tenantID),
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

func (a *BotAuth) validateJWT(tokenString string) error {
	parser := jwtv5.NewParser(
		jwtv5.WithAudience(a.appID),
		jwtv5.WithIssuedAt(),
	)

	token, _, err := parser.ParseUnverified(tokenString, jwtv5.MapClaims{})
	if err != nil {
		return fmt.Errorf("parse JWT: %w", err)
	}

	claims, ok := token.Claims.(jwtv5.MapClaims)
	if !ok {
		return fmt.Errorf("unexpected claims type")
	}

	iss, _ := claims["iss"].(string)
	validIssuers := []string{
		"https://api.botframework.com",
		"https://sts.windows.net/" + a.tenantID + "/",
		"https://login.microsoftonline.com/" + a.tenantID + "/v2.0",
	}
	issuerValid := false
	for _, valid := range validIssuers {
		if iss == valid {
			issuerValid = true
			break
		}
	}
	if !issuerValid {
		return fmt.Errorf("invalid issuer: %s", iss)
	}

	return nil
}
