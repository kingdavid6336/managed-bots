package gcalbot

import (
	"context"
	"fmt"

	"github.com/keybase/go-keybase-chat-bot/kbchat/types/chat1"
	"github.com/keybase/managed-bots/base"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

type InviteReaction string

const (
	InviteReactionYes   InviteReaction = "Yes 👍"
	InviteReactionNo    InviteReaction = "No 👎"
	InviteReactionMaybe InviteReaction = "Maybe 🤷"
)

type ResponseStatus string

const (
	ResponseStatusNeedsAction ResponseStatus = "needsAction"
	ResponseStatusDeclined    ResponseStatus = "declined"
	ResponseStatusTentative   ResponseStatus = "tentative"
	ResponseStatusAccepted    ResponseStatus = "accepted"
)

func (h *Handler) handleInvitesSubscribe(msg chat1.MsgSummary, args []string) error {
	if !base.IsDirectPrivateMessage(h.kbc.GetUsername(), msg) {
		h.ChatEcho(msg.ConvID, "This command can only be run through direct message.")
		return nil
	}

	if len(args) != 1 {
		h.ChatEcho(msg.ConvID, "Invalid number of arguments.")
		return nil
	}

	keybaseUsername := msg.Sender.Username
	accountNickname := args[0]
	accountID := GetAccountID(keybaseUsername, accountNickname)

	client, err := base.GetOAuthClient(accountID, msg, h.kbc, h.requests, h.config, h.db,
		h.getAccountOAuthOpts(msg, accountNickname))
	if err != nil || client == nil {
		// if no error, account doesn't exist, short circuit
		return err
	}

	srv, err := calendar.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return err
	}

	primaryCalendar, err := getPrimaryCalendar(srv)
	if err != nil {
		return err
	}

	exists, err := h.createSubscription(srv, Subscription{
		AccountID:     accountID,
		CalendarID:    primaryCalendar.Id,
		KeybaseConvID: msg.ConvID,
		Type:          SubscriptionTypeInvite,
	})
	if err != nil || exists {
		// if no error, subscription exists, short circuit
		return err
	}

	h.ChatEcho(msg.ConvID,
		"OK, you will be notified of event invites for your primary calendar '%s' from now on.", primaryCalendar.Summary)
	return nil
}

func (h *Handler) handleInvitesUnsubscribe(msg chat1.MsgSummary, args []string) error {
	if !base.IsDirectPrivateMessage(h.kbc.GetUsername(), msg) {
		h.ChatEcho(msg.ConvID, "This command can only be run through direct message.")
		return nil
	}

	if len(args) != 1 {
		h.ChatEcho(msg.ConvID, "Invalid number of arguments.")
		return nil
	}

	keybaseUsername := msg.Sender.Username
	accountNickname := args[0]
	accountID := GetAccountID(keybaseUsername, accountNickname)

	token, err := h.db.GetToken(accountID)
	if err != nil || token == nil {
		// if no error, account doesn't exist, short circuit
		return err
	}

	client := h.config.Client(context.Background(), token)
	srv, err := calendar.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return err
	}

	primaryCalendar, err := getPrimaryCalendar(srv)
	if err != nil {
		return err
	}

	exists, err := h.removeSubscription(srv, Subscription{
		AccountID:     accountID,
		CalendarID:    primaryCalendar.Id,
		KeybaseConvID: msg.ConvID,
		Type:          SubscriptionTypeInvite,
	})
	if err != nil || !exists {
		// if no error, subscription doesn't exist, short circuit
		return err
	}

	h.ChatEcho(msg.ConvID,
		"OK, you will no longer be notified of event invites for your primary calendar '%s'.", primaryCalendar.Summary)
	return nil
}

func (h *Handler) sendEventInvite(srv *calendar.Service, channel *Channel, event *calendar.Event) error {
	message := `You've been invited to %s: %s
Awaiting your response. *Are you going?*`

	var eventType string
	if event.Recurrence == nil {
		eventType = "an event"
	} else {
		eventType = "a recurring event"
	}

	timezone, err := GetUserTimezone(srv)
	if err != nil {
		return err
	}
	format24HourTime, err := GetUserFormat24HourTime(srv)
	if err != nil {
		return err
	}
	account, err := h.db.GetAccountByAccountID(channel.AccountID)
	if err != nil {
		return err
	}
	invitedCalendar, err := srv.Calendars.Get(channel.CalendarID).Do()
	if err != nil {
		return err
	}
	eventContent, err := FormatEvent(event, account.AccountNickname, invitedCalendar.Summary, timezone, format24HourTime)
	if err != nil {
		return err
	}

	sendRes, err := h.kbc.SendMessageByTlfName(account.KeybaseUsername, message, eventType, eventContent)
	if err != nil {
		return err
	}

	err = h.db.InsertInvite(Invite{
		AccountID:       channel.AccountID,
		CalendarID:      invitedCalendar.Id,
		EventID:         event.Id,
		KeybaseUsername: account.KeybaseUsername,
		MessageID:       uint(*sendRes.Result.MessageID),
	})
	if err != nil {
		return err
	}

	for _, reaction := range []InviteReaction{InviteReactionYes, InviteReactionNo, InviteReactionMaybe} {
		_, err = h.kbc.ReactByChannel(chat1.ChatChannel{Name: account.KeybaseUsername},
			*sendRes.Result.MessageID, string(reaction))
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *Handler) updateEventResponseStatus(invite *Invite, reaction InviteReaction) error {
	var responseStatus ResponseStatus
	var confirmationMessageStatus string
	switch reaction {
	case InviteReactionYes:
		responseStatus = ResponseStatusAccepted
		confirmationMessageStatus = "Going"
	case InviteReactionNo:
		responseStatus = ResponseStatusDeclined
		confirmationMessageStatus = "Not Going"
	case InviteReactionMaybe:
		responseStatus = ResponseStatusTentative
		confirmationMessageStatus = "Maybe Going"
	default:
		// reaction is not valid for responding to the event
		return nil
	}

	token, err := h.db.GetToken(invite.AccountID)
	if err != nil {
		return err
	} else if token == nil {
		h.Debug("token not found for '%s'", invite.AccountID)
		return nil
	}

	client := h.config.Client(context.Background(), token)
	srv, err := calendar.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return err
	}

	// fetch event
	// TODO(marcel): check if event was deleted
	event, err := srv.Events.Get(invite.CalendarID, invite.EventID).Fields("attendees").Do()
	if err != nil {
		return err
	}

	// update response status on event
	shouldPatch := false
	for index := range event.Attendees {
		if event.Attendees[index].Self {
			event.Attendees[index].ResponseStatus = string(responseStatus)
			shouldPatch = true
			break
		}
	}

	if !shouldPatch {
		return nil
	}

	// patch event to reflect new response status
	event, err = srv.Events.Patch(invite.CalendarID, invite.EventID, event).Fields("summary").Do()
	if err != nil {
		return err
	}

	account, err := h.db.GetAccountByAccountID(invite.AccountID)
	if err != nil {
		return err
	}
	invitedCalendar, err := srv.Calendars.Get(invite.CalendarID).Do()
	if err != nil {
		return err
	}
	accountCalendar := fmt.Sprintf("%s [%s]", invitedCalendar.Summary, account.AccountNickname)

	_, err = h.kbc.SendMessageByTlfName(invite.KeybaseUsername, "I've set your status as *%s* for event *%s* on calendar %s.",
		confirmationMessageStatus, event.Summary, accountCalendar)
	if err != nil {
		return err
	}

	return nil
}
