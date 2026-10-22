package composer

import (
	"strings"

	"smart-campaign-composer/backend/internal/domain"
)

// Field names the three pieces of content the LLM produces. The values are
// also the JSON keys of the tool input.
type Field string

const (
	FieldSMS          Field = "sms_content"
	FieldEmailSubject Field = "email_subject"
	FieldEmailBody    Field = "email_body"
)

// Fields lists every field in the order the model is asked to emit them
// (SMS first, so the shortest content appears first in the UI).
var Fields = []Field{FieldSMS, FieldEmailSubject, FieldEmailBody}

func (f Field) valid() bool {
	return f == FieldSMS || f == FieldEmailSubject || f == FieldEmailBody
}

// Message is the structured LLM output: one template per field. Templates
// may contain whitelisted {{variables}}.
type Message struct {
	SMSContent   string `json:"sms_content"`
	EmailSubject string `json:"email_subject"`
	EmailBody    string `json:"email_body"`
}

// Get returns the content of field f.
func (m Message) Get(f Field) string {
	switch f {
	case FieldSMS:
		return m.SMSContent
	case FieldEmailSubject:
		return m.EmailSubject
	case FieldEmailBody:
		return m.EmailBody
	}
	return ""
}

func (m *Message) set(f Field, v string) {
	switch f {
	case FieldSMS:
		m.SMSContent = v
	case FieldEmailSubject:
		m.EmailSubject = v
	case FieldEmailBody:
		m.EmailBody = v
	}
}

// Normalize canonicalises whitespace without changing meaning: CRLF/CR
// become LF and leading/trailing whitespace is trimmed. It is applied to
// LLM output and should be applied to marketer edits before validation.
func Normalize(m Message) Message {
	for _, f := range Fields {
		v := strings.ReplaceAll(m.Get(f), "\r\n", "\n")
		v = strings.ReplaceAll(v, "\r", "\n")
		m.set(f, strings.TrimSpace(v))
	}
	return m
}

// BodyFormat is the markup of the e-mail body.
type BodyFormat string

const (
	BodyMarkdown BodyFormat = "markdown"
	BodyHTML     BodyFormat = "html"
)

// FieldStats describes one validated field for the UI's live counters.
type FieldStats struct {
	// Runes is the raw template length in Unicode code points.
	Runes int `json:"runes"`
	// EstimatedRunes replaces every variable with its estimated width.
	EstimatedRunes int `json:"estimated_runes"`
	// Limit is the channel limit (0 = no business limit).
	Limit     int      `json:"limit,omitempty"`
	Variables []string `json:"variables,omitempty"`
}

// Report is the validator's view of a message.
type Report struct {
	SMS             FieldStats `json:"sms_content"`
	EmailSubject    FieldStats `json:"email_subject"`
	EmailBody       FieldStats `json:"email_body"`
	EmailBodyFormat BodyFormat `json:"email_body_format"`
}

// Draft is a validated message ready for human review.
type Draft struct {
	Message  Message `json:"message"`
	Report   Report  `json:"report"`
	Attempts int     `json:"attempts"`
}

// CampaignMessages maps a validated message onto the persisted domain model:
// one row per channel, in draft status. SMS never carries a subject and
// e-mail always does, matching the campaign_messages CHECK constraint.
func (m Message) CampaignMessages(campaignID int64) []domain.CampaignMessage {
	return []domain.CampaignMessage{
		{CampaignID: campaignID, Channel: domain.ChannelSMS, Content: m.SMSContent, Status: domain.MessageStatusDraft},
		{CampaignID: campaignID, Channel: domain.ChannelEmail, Content: m.EmailBody, Subject: m.EmailSubject, Status: domain.MessageStatusDraft},
	}
}

// MessageFromCampaign rebuilds a Message from persisted rows (e.g. to
// re-validate or render an approved campaign). Unknown channels are ignored.
func MessageFromCampaign(rows []domain.CampaignMessage) Message {
	var m Message
	for _, r := range rows {
		switch r.Channel {
		case domain.ChannelSMS:
			m.SMSContent = r.Content
		case domain.ChannelEmail:
			m.EmailSubject, m.EmailBody = r.Subject, r.Content
		}
	}
	return m
}
