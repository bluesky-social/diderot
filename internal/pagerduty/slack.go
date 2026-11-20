package pagerduty

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/ask"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/google/uuid"
	"github.com/slack-go/slack"
)

var ErrSlackThreadNotFound = errors.New("PagerDuty alert thread not yet found in the alert channel")

var incidentLink = regexp.MustCompile(`https://[^\s<>"\\|]+`)

type AnswerDelivery interface {
	DeliverAskAnswer(context.Context, string, string, uuid.UUID, ask.TurnResult, string) (string, error)
	DeliverTrustedReply(context.Context, string, string, string, string) (string, error)
}

type ArtifactLoader interface {
	LoadResultArtifacts(context.Context, uuid.UUID, int) ([]ask.ArtifactDescriptor, error)
}

type SlackDelivery struct {
	API       *slack.Client
	Limiter   *slackcx.Limiter
	Store     *Store
	ChannelID string
	// ChannelName, when set, must match the name of ChannelID, so a
	// mistyped ID cannot route investigations into some other channel.
	ChannelName string
	Answers     AnswerDelivery
	Artifacts   ArtifactLoader
	// Scopes bounds which agent sessions may turn into related-work
	// mentions; it must match the investigation's Ask audience.
	Scopes []string
}

func (delivery *SlackDelivery) Deliver(ctx context.Context, job *Job, report string) (string, error) {
	if delivery.API == nil || delivery.Store == nil || delivery.Store.pool == nil || delivery.ChannelID == "" || job.SessionID == nil || delivery.Answers == nil || delivery.Artifacts == nil {
		return "", errors.New("slack delivery requires API, store, alert channel ID, and session")
	}
	var channel *slack.Channel
	err := slackcx.WithSlackRetry(ctx, delivery.Limiter, "conversations.info", func() error {
		var apiErr error
		channel, apiErr = delivery.API.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: delivery.ChannelID})
		return apiErr
	})
	if err != nil {
		return "", err
	}
	if (delivery.ChannelName != "" && channel.Name != delivery.ChannelName) || channel.IsExtShared || channel.IsPendingExtShared || channel.IsArchived || !channel.IsMember {
		return "", fmt.Errorf("slack destination must be the internal #%s channel with Diderot as a member", cmp.Or(delivery.ChannelName, channel.Name))
	}
	if job.SlackThreadTS == "" {
		threadTS, err := delivery.findThread(ctx, job.Event)
		if err != nil {
			return "", err
		}
		if err := delivery.Store.SaveSlackTarget(ctx, job, delivery.ChannelID, threadTS); err != nil {
			return "", err
		}
		job.SlackChannelID, job.SlackThreadTS = delivery.ChannelID, threadTS
	}
	if job.SlackChannelID != delivery.ChannelID {
		return "", errors.New("saved Slack destination differs from the configured alert channel")
	}
	if _, err := delivery.Store.pool.Exec(ctx, `INSERT INTO ask_slack_sessions (channel_id, thread_ts, session_id)
		VALUES ($1,$2,$3) ON CONFLICT (channel_id, thread_ts) DO NOTHING`, delivery.ChannelID, job.SlackThreadTS, *job.SessionID); err != nil {
		return "", fmt.Errorf("attach investigation to Slack conversation: %w", err)
	}
	artifacts, err := delivery.Artifacts.LoadResultArtifacts(ctx, *job.SessionID, job.AssistantTurn)
	if err != nil {
		return "", err
	}
	messageTS, err := delivery.Answers.DeliverAskAnswer(ctx, delivery.ChannelID, job.SlackThreadTS, *job.SessionID,
		ask.TurnResult{Reply: report, AssistantTurn: job.AssistantTurn, Artifacts: artifacts},
		"Investigation reference: diderot-pd-"+sha256EventID(job.Event.ID))
	if err != nil {
		return messageTS, err
	}
	// Resolved at delivery, not analysis, so a session deleted or made
	// owner-only in between is never mentioned.
	owners, err := delivery.Store.relatedWorkOwners(ctx, report, alertTime(job.Event), delivery.Scopes)
	if err != nil {
		return messageTS, fmt.Errorf("resolve related agent work: %w", err)
	}
	if len(owners) > 0 {
		if _, err := delivery.Answers.DeliverTrustedReply(ctx, delivery.ChannelID, job.SlackThreadTS,
			relatedWorkMessage(owners), "Related-work reference: diderot-pd-"+sha256EventID(job.Event.ID)); err != nil {
			return messageTS, fmt.Errorf("post related agent work: %w", err)
		}
	}
	return messageTS, nil
}

func (delivery *SlackDelivery) findThread(ctx context.Context, event Event) (string, error) {
	start := event.OccurredAt
	if !event.Data.CreatedAt.IsZero() && event.Data.CreatedAt.Before(start) {
		start = event.Data.CreatedAt
	}
	parameters := &slack.GetConversationHistoryParameters{
		ChannelID: delivery.ChannelID, Oldest: strconv.FormatInt(start.Add(-5*time.Minute).Unix(), 10),
		Latest: strconv.FormatInt(time.Now().Unix()+1, 10), Limit: 100, Inclusive: true,
	}
	matches := map[string]bool{}
	for page := 0; page < 100; page++ {
		var response *slack.GetConversationHistoryResponse
		err := slackcx.WithSlackRetry(ctx, delivery.Limiter, "conversations.history", func() error {
			var apiErr error
			response, apiErr = delivery.API.GetConversationHistoryContext(ctx, parameters)
			return apiErr
		})
		if err != nil {
			return "", err
		}
		if response.Messages == nil {
			return "", errors.New("slack history response missing messages array")
		}
		for _, message := range response.Messages {
			if matchesIncident(message, event) {
				matches[message.Timestamp] = true
			}
		}
		if !response.HasMore && response.ResponseMetaData.NextCursor == "" {
			if len(matches) > 1 {
				return "", errors.New("multiple alert-channel roots link to this incident; refusing ambiguous thread delivery")
			}
			for threadTS := range matches {
				return threadTS, nil
			}
			return "", ErrSlackThreadNotFound
		}
		cursor := response.ResponseMetaData.NextCursor
		if cursor == "" || cursor == parameters.Cursor {
			return "", errors.New("slack history pagination did not advance")
		}
		parameters.Cursor = cursor
	}
	return "", errors.New("slack alert lookup exceeded 100 history pages; coverage incomplete")
}

func matchesIncident(message slack.Message, event Event) bool {
	if message.Timestamp == "" || (message.ThreadTimestamp != "" && message.ThreadTimestamp != message.Timestamp) ||
		message.SubType == "thread_broadcast" || (message.BotID == "" && message.SubType != "bot_message") {
		return false
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return false
	}
	incidentURL, _ := url.Parse(event.Data.HTMLURL)
	for _, link := range incidentLink.FindAllString(string(encoded), -1) {
		parsed, err := url.Parse(link)
		if err != nil || parsed.User != nil || parsed.Port() != "" || parsed.Path != "/incidents/"+event.Data.ID {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		if host == "pagerduty.com" || strings.HasSuffix(host, ".pagerduty.com") ||
			(incidentURL != nil && incidentURL.Scheme == "https" && strings.EqualFold(host, incidentURL.Hostname())) {
			return true
		}
	}
	return false
}
