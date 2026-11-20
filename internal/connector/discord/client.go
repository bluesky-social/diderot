package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/bluesky-social/diderot/internal/ratelimit"
	"github.com/bluesky-social/diderot/internal/safehttp"
)

// DefaultAPIBase is Discord's REST API, version 10.
const DefaultAPIBase = "https://discord.com/api/v10"

// userAgent is the form Discord requires of a bot's requests.
const userAgent = "DiscordBot (https://github.com/bluesky-social/diderot, 1)"

// maxAttempts bounds the tries of one request through rate limits and
// server errors.
const maxAttempts = 6

// APIError is a Discord answer other than success. It never carries the
// response body, which can echo what was sent.
type APIError struct {
	Status int
	// Code is Discord's JSON error code, when the body has one.
	Code int
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("discord: HTTP %d (error %d)", e.Status, e.Code)
	}
	return fmt.Sprintf("discord: HTTP %d", e.Status)
}

// isStatus reports whether err is a Discord answer with one of statuses.
func isStatus(err error, statuses ...int) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, status := range statuses {
		if apiErr.Status == status {
			return true
		}
	}
	return false
}

// Client is a bot's REST client. It keeps to Discord's rate limits: the
// global limit with a token bucket a little under 50 requests a second, and
// each route's bucket from the X-RateLimit headers, waiting out a 429's
// retry_after before trying again.
type Client struct {
	base  string
	token string
	http  *http.Client

	global *ratelimit.Bucket

	mu sync.Mutex
	// routes holds, per route key, when its bucket next has room.
	routes map[string]time.Time
}

// NewClient returns a client for the bot token against base (DefaultAPIBase
// unless a test points it elsewhere).
func NewClient(base, token string) *Client {
	if base == "" {
		base = DefaultAPIBase
	}
	return &Client{
		base: strings.TrimRight(base, "/"), token: token,
		http:   safehttp.NewClient(safehttp.Options{Timeout: 30 * time.Second, FailOnRedirect: true}),
		global: ratelimit.New(rate.Limit(45), 10),
		routes: map[string]time.Time{},
	}
}

// Format keeps the token out of every fmt verb.
func (c *Client) Format(f fmt.State, _ rune) { _, _ = fmt.Fprintf(f, "discord.Client{%s}", c.base) }

// snowflakeSegment finds the IDs in a path, for its route key.
var snowflakeSegment = regexp.MustCompile(`/\d{5,}`)

// routeKey is the rate-limit route of a request: its method and path with
// every ID but the major one (the first, a channel or guild) replaced.
func routeKey(method, path string) string {
	first := true
	return method + " " + snowflakeSegment.ReplaceAllStringFunc(path, func(id string) string {
		if first {
			first = false
			return id
		}
		return "/:id"
	})
}

// get reads path, with query, into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// requestBody is what a write sends: JSON, or a multipart form with files.
type requestBody struct {
	contentType string
	data        []byte
}

// jsonBody is v as a request body.
func jsonBody(v any) (*requestBody, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &requestBody{contentType: "application/json", data: data}, nil
}

// do sends one request and reads a JSON answer into out (when out is not
// nil and the answer has a body). A read is tried again through server
// errors and dropped connections; a write only through rate limits, which
// Discord answers before acting, so a write is never made twice.
func (c *Client) do(ctx context.Context, method, path string, body *requestBody, out any) error {
	target := c.base + path
	shown := redactPath(path)
	routePath, _, _ := strings.Cut(path, "?")
	key := routeKey(method, routePath)
	retryable := method == http.MethodGet
	for attempt := 1; ; attempt++ {
		if err := c.waitRoute(ctx, key); err != nil {
			return err
		}
		if err := c.global.Wait(ctx); err != nil {
			return err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body.data)
		}
		request, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bot "+c.token)
		request.Header.Set("User-Agent", userAgent)
		if body != nil {
			request.Header.Set("Content-Type", body.contentType)
		}
		response, err := c.http.Do(request)
		if err != nil {
			if !retryable || ctx.Err() != nil || attempt >= maxAttempts {
				return fmt.Errorf("discord: %s %s: %w", method, shown, scrubURLError(err))
			}
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return err
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		_ = response.Body.Close()
		c.observe(key, response.Header)
		switch {
		case response.StatusCode == http.StatusTooManyRequests:
			wait, global := retryAfter(response.Header, data)
			if global {
				c.global.PauseUntil(time.Now().Add(wait))
			} else {
				c.blockRoute(key, time.Now().Add(wait))
			}
			if attempt >= maxAttempts {
				return &APIError{Status: response.StatusCode}
			}
			continue
		case response.StatusCode >= 500 && retryable:
			if attempt >= maxAttempts {
				return &APIError{Status: response.StatusCode}
			}
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return err
			}
			continue
		case response.StatusCode < 200 || response.StatusCode > 299:
			var failure struct {
				Code int `json:"code"`
			}
			_ = json.Unmarshal(data, &failure)
			return &APIError{Status: response.StatusCode, Code: failure.Code}
		}
		if readErr != nil {
			return fmt.Errorf("discord: %s %s: read: %w", method, shown, readErr)
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("discord: %s %s: decode: %w", method, shown, err)
		}
		return nil
	}
}

// tokenPath finds an interaction's or a webhook's token in a path.
var tokenPath = regexp.MustCompile(`^(/(?:interactions|webhooks)/\d+/)[^/?]+`)

// redactPath is path as errors show it: without an interaction's token,
// which answers for the interaction while it lasts.
func redactPath(path string) string {
	path, _, _ = strings.Cut(path, "?")
	return tokenPath.ReplaceAllString(path, "${1}<token>")
}

// scrubURLError drops the request URL from a transport error; the URL
// holds nothing secret, but the error is shown on pages.
func scrubURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func (c *Client) waitRoute(ctx context.Context, key string) error {
	c.mu.Lock()
	until := c.routes[key]
	c.mu.Unlock()
	if d := time.Until(until); d > 0 {
		return sleep(ctx, d)
	}
	return nil
}

func (c *Client) blockRoute(key string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.routes[key]) {
		c.routes[key] = until
	}
}

// observe blocks key until its bucket resets when a response says it has no
// requests left.
func (c *Client) observe(key string, header http.Header) {
	if header.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	resetAfter, err := strconv.ParseFloat(header.Get("X-RateLimit-Reset-After"), 64)
	if err != nil || resetAfter <= 0 {
		return
	}
	c.blockRoute(key, time.Now().Add(time.Duration(resetAfter*float64(time.Second))))
}

// retryAfter reads a 429: how long to wait, and whether the limit is the
// global one.
func retryAfter(header http.Header, body []byte) (time.Duration, bool) {
	var limited struct {
		RetryAfter float64 `json:"retry_after"`
		Global     bool    `json:"global"`
	}
	_ = json.Unmarshal(body, &limited)
	seconds := limited.RetryAfter
	if seconds <= 0 {
		seconds, _ = strconv.ParseFloat(header.Get("Retry-After"), 64)
	}
	if seconds <= 0 {
		seconds = 1
	}
	global := limited.Global || header.Get("X-RateLimit-Global") == "true"
	return time.Duration(seconds * float64(time.Second)), global
}

func backoff(attempt int) time.Duration {
	return min(time.Duration(1<<attempt)*250*time.Millisecond, 15*time.Second)
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// The endpoints the connector reads.

func (c *Client) currentUser(ctx context.Context) (user, error) {
	var u user
	return u, c.get(ctx, "/users/@me", nil, &u)
}

func (c *Client) user(ctx context.Context, userID string) (user, error) {
	var u user
	return u, c.get(ctx, "/users/"+userID, nil, &u)
}

func (c *Client) currentApplication(ctx context.Context) (application, error) {
	var app application
	return app, c.get(ctx, "/applications/@me", nil, &app)
}

func (c *Client) guilds(ctx context.Context) ([]guild, error) {
	var all []guild
	query := url.Values{"limit": {"200"}}
	for {
		var page []guild
		if err := c.get(ctx, "/users/@me/guilds", query, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 200 {
			return all, nil
		}
		query.Set("after", page[len(page)-1].ID)
	}
}

func (c *Client) guildChannels(ctx context.Context, guildID string) ([]channel, error) {
	var channels []channel
	return channels, c.get(ctx, "/guilds/"+guildID+"/channels", nil, &channels)
}

func (c *Client) guildRoles(ctx context.Context, guildID string) ([]role, error) {
	var roles []role
	return roles, c.get(ctx, "/guilds/"+guildID+"/roles", nil, &roles)
}

func (c *Client) guild(ctx context.Context, guildID string) (guild, error) {
	var g guild
	return g, c.get(ctx, "/guilds/"+guildID, nil, &g)
}

func (c *Client) guildMember(ctx context.Context, guildID, userID string) (member, error) {
	var m member
	return m, c.get(ctx, "/guilds/"+guildID+"/members/"+userID, nil, &m)
}

func (c *Client) activeThreads(ctx context.Context, guildID string) ([]channel, error) {
	var active activeThreads
	return active.Threads, c.get(ctx, "/guilds/"+guildID+"/threads/active", nil, &active)
}

// archivedPublicThreads pages a channel's archived public threads, most
// recently archived first.
func (c *Client) archivedPublicThreads(ctx context.Context, channelID string, visit func([]channel) (bool, error)) error {
	query := url.Values{"limit": {"100"}}
	for {
		var page archivedThreads
		if err := c.get(ctx, "/channels/"+channelID+"/threads/archived/public", query, &page); err != nil {
			return err
		}
		more, err := visit(page.Threads)
		if err != nil || !more || !page.HasMore || len(page.Threads) == 0 {
			return err
		}
		last := page.Threads[len(page.Threads)-1]
		if last.ThreadMetadata == nil || last.ThreadMetadata.ArchiveTimestamp == "" {
			return errors.New("discord: archived thread without an archive time")
		}
		query.Set("before", last.ThreadMetadata.ArchiveTimestamp)
	}
}

func (c *Client) channel(ctx context.Context, channelID string) (channel, error) {
	var ch channel
	return ch, c.get(ctx, "/channels/"+channelID, nil, &ch)
}

func (c *Client) message(ctx context.Context, channelID, messageID string) (message, error) {
	var m message
	return m, c.get(ctx, "/channels/"+channelID+"/messages/"+messageID, nil, &m)
}

// messagesBefore pages a channel's messages newest first, starting before
// the message before ("" for the newest), until visit says stop.
func (c *Client) messagesBefore(ctx context.Context, channelID, before string, visit func([]message) (bool, error)) error {
	query := url.Values{"limit": {"100"}}
	for {
		if before != "" {
			query.Set("before", before)
		}
		var page []message
		if err := c.get(ctx, "/channels/"+channelID+"/messages", query, &page); err != nil {
			return err
		}
		more, err := visit(page)
		if err != nil || !more || len(page) < 100 {
			return err
		}
		before = page[len(page)-1].ID
	}
}

// messagesAfter reads a channel's messages after the message after, oldest
// first, a page at a time.
func (c *Client) messagesAfter(ctx context.Context, channelID, after string, visit func([]message) error) error {
	query := url.Values{"limit": {"100"}}
	for {
		query.Set("after", after)
		var page []message
		if err := c.get(ctx, "/channels/"+channelID+"/messages", query, &page); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		// Discord answers newest first whatever the cursor; sort to be
		// sure of the order.
		oldestFirst := slices.Clone(page)
		slices.SortFunc(oldestFirst, func(a, b message) int {
			switch {
			case laterID(a.ID, b.ID):
				return 1
			case laterID(b.ID, a.ID):
				return -1
			}
			return 0
		})
		if err := visit(oldestFirst); err != nil {
			return err
		}
		after = oldestFirst[len(oldestFirst)-1].ID
		if len(page) < 100 {
			return nil
		}
	}
}

// gatewayURL is where the bot's gateway connections go.
func (c *Client) gatewayURL(ctx context.Context) (string, error) {
	var gateway struct {
		URL string `json:"url"`
	}
	if err := c.get(ctx, "/gateway/bot", nil, &gateway); err != nil {
		return "", err
	}
	if gateway.URL == "" {
		return "", errors.New("discord: no gateway URL")
	}
	return gateway.URL, nil
}
