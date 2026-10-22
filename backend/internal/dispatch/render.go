package dispatch

import (
	"fmt"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/domain"
)

// ComposerRenderer adapts composer.Renderer (ADR 006) to Renderer: it fills
// {{variables}} for one user and re-checks the hard channel limits on the
// real text (SMS ≤ 160, subject ≤ 80 code points).
//
// Rendering must be deterministic for a given user: a resumed delivery is
// re-sent with the same idempotency key, and a provider rejects a key reused
// with a different payload (409) instead of sending twice.
type ComposerRenderer struct {
	R *composer.Renderer
}

// NewComposerRenderer uses composer.DefaultRenderer when r is nil.
func NewComposerRenderer(r *composer.Renderer) ComposerRenderer {
	if r == nil {
		r = composer.DefaultRenderer
	}
	return ComposerRenderer{R: r}
}

// Render personalises m for u. The caller sets the idempotency key.
func (c ComposerRenderer) Render(m domain.CampaignMessage, u domain.User) (Message, error) {
	switch m.Channel {
	case domain.ChannelSMS:
		body, err := c.R.RenderSMS(m.Content, u)
		if err != nil {
			return Message{}, err
		}
		return Message{To: u.Phone, Body: body}, nil
	case domain.ChannelEmail:
		subject, body, format, err := c.R.RenderEmail(m.Subject, m.Content, u)
		if err != nil {
			return Message{}, err
		}
		return Message{To: u.Email, Subject: subject, Body: body, BodyFormat: string(format)}, nil
	}
	return Message{}, fmt.Errorf("unsupported channel %q", m.Channel)
}
