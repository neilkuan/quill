package teams

type Activity struct {
	Type         string       `json:"type"`
	ID           string       `json:"id,omitempty"`
	Timestamp    string       `json:"timestamp,omitempty"`
	ServiceURL   string       `json:"serviceUrl,omitempty"`
	ChannelID    string       `json:"channelId,omitempty"`
	From         Account      `json:"from,omitempty"`
	Conversation Conversation `json:"conversation,omitempty"`
	Recipient    Account      `json:"recipient,omitempty"`
	Text         string       `json:"text,omitempty"`
	TextFormat   string       `json:"textFormat,omitempty"`
	Attachments  []Attachment `json:"attachments,omitempty"`
	Entities     []Entity     `json:"entities,omitempty"`
	ReplyToID    string       `json:"replyToId,omitempty"`
}

type Account struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type Conversation struct {
	ID string `json:"id,omitempty"`
}

type Attachment struct {
	ContentType string `json:"contentType,omitempty"`
	ContentURL  string `json:"contentUrl,omitempty"`
	Name        string `json:"name,omitempty"`
}

type Entity struct {
	Type      string   `json:"type,omitempty"`
	Mentioned *Account `json:"mentioned,omitempty"`
	Text      string   `json:"text,omitempty"`
}
