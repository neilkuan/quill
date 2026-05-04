package teams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type BotClient struct {
	auth *BotAuth
	http *http.Client
}

func NewBotClient(auth *BotAuth) *BotClient {
	return &BotClient{
		auth: auth,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *BotClient) SendActivity(serviceURL, conversationID string, activity *Activity) (*Activity, error) {
	url := fmt.Sprintf("%s/v3/conversations/%s/activities",
		strings.TrimRight(serviceURL, "/"), conversationID)
	return c.doActivityRequest(http.MethodPost, url, activity)
}

func (c *BotClient) UpdateActivity(serviceURL, conversationID, activityID string, activity *Activity) error {
	url := fmt.Sprintf("%s/v3/conversations/%s/activities/%s",
		strings.TrimRight(serviceURL, "/"), conversationID, activityID)
	_, err := c.doActivityRequest(http.MethodPut, url, activity)
	return err
}

func (c *BotClient) SendTyping(serviceURL, conversationID string, from Account) error {
	activity := &Activity{
		Type: "typing",
		From: from,
	}
	_, err := c.SendActivity(serviceURL, conversationID, activity)
	return err
}

// ConversationMember describes a Teams channel/chat member as returned
// by GET /v3/conversations/{id}/members. Bot Framework calls these
// "ChannelAccount"s — `id` is the `29:xxx` user identifier needed to
// build a working mention entity, `name` is the rendered display name.
// Teams-extension fields (givenName / surname / email / aadObjectId /
// userPrincipalName) are passed through so future code can match users
// by alternate keys without re-fetching.
type ConversationMember struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	GivenName         string `json:"givenName,omitempty"`
	Surname           string `json:"surname,omitempty"`
	Email             string `json:"email,omitempty"`
	AADObjectID       string `json:"aadObjectId,omitempty"`
	UserPrincipalName string `json:"userPrincipalName,omitempty"`
}

// GetConversationMembers fetches the full member roster for a Teams
// conversation. For channel/group chats this returns every user in the
// channel; for 1:1 personal scope it returns just the user (the bot
// itself is not listed). Used to pre-populate the mention directory so
// the bot can @ a user before that user has spoken in the channel.
//
// The Bot Framework token returned by GetBotToken doubles as the bearer
// for this read endpoint.
func (c *BotClient) GetConversationMembers(serviceURL, conversationID string) ([]ConversationMember, error) {
	url := fmt.Sprintf("%s/v3/conversations/%s/members",
		strings.TrimRight(serviceURL, "/"), conversationID)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	token, err := c.auth.GetBotToken()
	if err != nil {
		return nil, fmt.Errorf("get bot token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	var members []ConversationMember
	if len(body) == 0 {
		return members, nil
	}
	if err := json.Unmarshal(body, &members); err != nil {
		return nil, fmt.Errorf("decode members: %w", err)
	}
	return members, nil
}

func (c *BotClient) doActivityRequest(method, url string, activity *Activity) (*Activity, error) {
	body, err := json.Marshal(activity)
	if err != nil {
		return nil, fmt.Errorf("marshal activity: %w", err)
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	token, err := c.auth.GetBotToken()
	if err != nil {
		return nil, fmt.Errorf("get bot token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result Activity
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &result); err != nil {
			return &Activity{}, nil
		}
	}

	return &result, nil
}
