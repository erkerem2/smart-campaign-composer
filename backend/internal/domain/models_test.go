package domain

import "testing"

func TestChannelValid(t *testing.T) {
	for _, ch := range Channels {
		if !ch.Valid() {
			t.Errorf("%q should be valid", ch)
		}
	}
	for _, ch := range []Channel{"", "push", "SMS"} {
		if ch.Valid() {
			t.Errorf("%q should be invalid", ch)
		}
	}
}

func TestCampaignStatusTransitions(t *testing.T) {
	tests := []struct {
		from, to CampaignStatus
		want     bool
	}{
		{CampaignStatusDraft, CampaignStatusApproved, true},
		{CampaignStatusApproved, CampaignStatusSending, true},
		{CampaignStatusSending, CampaignStatusCompleted, true},
		{CampaignStatusSending, CampaignStatusFailed, true},
		// Human approval cannot be skipped.
		{CampaignStatusDraft, CampaignStatusSending, false},
		{CampaignStatusDraft, CampaignStatusCompleted, false},
		// Terminal states stay terminal.
		{CampaignStatusCompleted, CampaignStatusSending, false},
		{CampaignStatusFailed, CampaignStatusDraft, false},
		{CampaignStatusApproved, CampaignStatusDraft, false},
	}
	for _, tt := range tests {
		if got := tt.from.CanTransitionTo(tt.to); got != tt.want {
			t.Errorf("%s -> %s = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestStatusValid(t *testing.T) {
	if CampaignStatus("archived").Valid() {
		t.Error("unknown campaign status accepted")
	}
	if MessageStatus("sent").Valid() {
		t.Error("unknown message status accepted")
	}
	if DispatchStatus("pending").Valid() {
		t.Error("unknown dispatch status accepted")
	}
	if !CampaignStatusFailed.Valid() || !MessageStatusApproved.Valid() || !DispatchStatusSent.Valid() {
		t.Error("known status rejected")
	}
}

func TestUserCanReceive(t *testing.T) {
	u := User{OptInSMS: true, OptInEmail: false}
	if !u.CanReceive(ChannelSMS) {
		t.Error("opted-in SMS user should receive SMS")
	}
	if u.CanReceive(ChannelEmail) {
		t.Error("opted-out email user must not receive email")
	}
	if u.CanReceive(Channel("push")) {
		t.Error("unknown channel must never be deliverable")
	}
}

func TestIdempotencyKey(t *testing.T) {
	d := DispatchLog{CampaignID: 7, UserID: 1234, Channel: ChannelEmail}
	if got, want := d.IdempotencyKey(), "7:1234:email"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
