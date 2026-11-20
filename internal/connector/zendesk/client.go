package zendesk

import (
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

var subdomainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

var errProtocol = errors.New("zendesk: invalid source response")

type Client struct {
	origin        string
	clientID      string
	clientSecret  string
	tokenMu       sync.Mutex
	token         string
	tokenExpiry   time.Time
	httpClient    *http.Client
	limiter       *ratelimit.Bucket
	exportLimiter *ratelimit.Bucket
}

type APIError struct {
	Status     int
	RetryAfter time.Duration
}

func (err *APIError) Error() string {
	return fmt.Sprintf("zendesk: upstream HTTP %d", err.Status)
}

func ValidateCredentials(subdomain, clientID, clientSecret string) error {
	if !subdomainPattern.MatchString(subdomain) {
		return errors.New("zendesk: subdomain must be a lowercase DNS label")
	}
	if clientID == "" || strings.ContainsAny(clientID, "\r\n") || strings.TrimSpace(clientID) != clientID {
		return errors.New("zendesk: an OAuth client identifier is required")
	}
	if clientSecret == "" || strings.ContainsAny(clientSecret, "\r\n") || strings.TrimSpace(clientSecret) != clientSecret {
		return errors.New("zendesk: an OAuth client secret is required")
	}
	return nil
}

func NewClient(subdomain, clientID, clientSecret string, requestsPerMinute int) (*Client, error) {
	if err := ValidateCredentials(subdomain, clientID, clientSecret); err != nil {
		return nil, err
	}
	if requestsPerMinute <= 0 {
		return nil, errors.New("zendesk: requests per minute must be positive")
	}
	return &Client{
		origin: "https://" + subdomain + ".zendesk.com", clientID: clientID, clientSecret: clientSecret,
		httpClient:    safehttp.NewClient(safehttp.Options{Timeout: 45 * time.Second, CheckRedirect: safehttp.NoFollow}),
		limiter:       ratelimit.New(rate.Limit(requestsPerMinute)/60, 1),
		exportLimiter: ratelimit.New(rate.Every(7*time.Second), 1),
	}, nil
}

func (client *Client) get(ctx context.Context, path string, query url.Values, authenticated bool, target any) error {
	for attempt := 0; attempt < 3; attempt++ {
		if strings.HasPrefix(path, "/api/v2/incremental/") {
			if err := client.exportLimiter.Wait(ctx); err != nil {
				return err
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.origin+path+"?"+query.Encode(), nil)
		if err != nil {
			return errors.New("zendesk: invalid API request")
		}
		request.Header.Set("Accept", "application/json")
		response, err := client.do(request, client.httpClient, authenticated)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("zendesk: API transport failed")
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			failure := &APIError{Status: response.StatusCode, RetryAfter: retryDelay(response.Header.Get("Retry-After"))}
			if attempt < 2 && (failure.Status == 429 || failure.Status >= 500) {
				delay := max(failure.RetryAfter, time.Duration(1<<attempt)*time.Second)
				if delay > time.Minute {
					return failure
				}
				if err := sleep(ctx, delay); err != nil {
					return err
				}
				continue
			}
			return failure
		}
		const maximumBytes = 32 << 20
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			return errors.New("zendesk: incomplete API response")
		}
		if len(body) > maximumBytes {
			return fmt.Errorf("%w: oversized API response", errProtocol)
		}
		if err := json.Unmarshal(body, target); err != nil {
			return errProtocol
		}
		return nil
	}
	return errors.New("zendesk: API retry limit reached")
}

func retryDelay(value string) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if until, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(until))
	}
	return 0
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type User struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Ticket struct {
	ID             int64           `json:"id"`
	Subject        string          `json:"subject"`
	Description    string          `json:"description"`
	Status         string          `json:"status"`
	Priority       string          `json:"priority"`
	Type           string          `json:"type"`
	Tags           []string        `json:"tags"`
	RequesterID    int64           `json:"requester_id"`
	AssigneeID     int64           `json:"assignee_id"`
	GroupID        int64           `json:"group_id"`
	OrganizationID int64           `json:"organization_id"`
	CustomFields   json.RawMessage `json:"custom_fields"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type Attachment struct {
	ID         int64        `json:"id"`
	Filename   string       `json:"file_name"`
	ContentURL string       `json:"content_url"`
	Mimetype   string       `json:"content_type"`
	Size       int64        `json:"size"`
	Deleted    bool         `json:"deleted"`
	Thumbnails []Attachment `json:"thumbnails,omitempty"`
}

type Comment struct {
	ID          int64        `json:"id"`
	AuthorID    int64        `json:"author_id"`
	Body        string       `json:"body"`
	PlainBody   string       `json:"plain_body"`
	Public      bool         `json:"public"`
	CreatedAt   time.Time    `json:"created_at"`
	Attachments []Attachment `json:"attachments"`
}

type TicketPage struct {
	Tickets     []Ticket `json:"tickets"`
	Users       []User   `json:"users"`
	AfterCursor string   `json:"after_cursor"`
	EndOfStream *bool    `json:"end_of_stream"`
}

func (client *Client) Tickets(ctx context.Context, cursor string) (TicketPage, error) {
	return client.ticketsSince(ctx, cursor, 0)
}

func (client *Client) ticketsSince(ctx context.Context, cursor string, startTime int64) (TicketPage, error) {
	query := url.Values{"per_page": {"100"}, "include": {"users"}, "exclude_deleted": {"false"}}
	if cursor == "" {
		query.Set("start_time", strconv.FormatInt(startTime, 10))
	} else {
		query.Set("cursor", cursor)
	}
	var page TicketPage
	if err := client.get(ctx, "/api/v2/incremental/tickets/cursor.json", query, true, &page); err != nil {
		return page, err
	}
	if page.Tickets != nil && len(page.Tickets) == 0 && page.AfterCursor == "" && page.EndOfStream != nil && *page.EndOfStream {
		page.AfterCursor = cursor
	}
	if page.Tickets == nil || page.AfterCursor == "" || page.EndOfStream == nil || (page.AfterCursor == cursor && !*page.EndOfStream) {
		return page, errors.New("zendesk: incomplete ticket export pagination")
	}
	return page, nil
}

func (client *Client) Comments(ctx context.Context, ticketID int64) ([]Comment, []User, error) {
	var comments []Comment
	var users []User
	query := url.Values{"page[size]": {"100"}, "include": {"users"}}
	seen := make(map[string]bool)
	for {
		var page struct {
			Comments []Comment `json:"comments"`
			Users    []User    `json:"users"`
			Meta     struct {
				HasMore     *bool  `json:"has_more"`
				AfterCursor string `json:"after_cursor"`
			} `json:"meta"`
		}
		if err := client.get(ctx, fmt.Sprintf("/api/v2/tickets/%d/comments.json", ticketID), query, true, &page); err != nil {
			return nil, nil, err
		}
		if page.Comments == nil || page.Meta.HasMore == nil {
			return nil, nil, fmt.Errorf("%w: incomplete comment pagination", errProtocol)
		}
		comments = append(comments, page.Comments...)
		users = append(users, page.Users...)
		if !*page.Meta.HasMore {
			return comments, users, nil
		}
		if page.Meta.AfterCursor == "" || seen[page.Meta.AfterCursor] {
			return nil, nil, fmt.Errorf("%w: comment cursor did not advance", errProtocol)
		}
		seen[page.Meta.AfterCursor] = true
		query.Set("page[after]", page.Meta.AfterCursor)
	}
}

// searchTicketIDs lists the tickets matching a search filter (such as
// "group:123" or "group:none") through the cursor-paged search export, which
// has no 1,000-result cap.
func (client *Client) searchTicketIDs(ctx context.Context, filter string) ([]int64, error) {
	query := url.Values{"query": {"type:ticket " + filter}, "filter[type]": {"ticket"}, "page[size]": {"1000"}}
	var ids []int64
	err := client.cursorPages(ctx, "/api/v2/search/export.json", query, func(raw json.RawMessage) error {
		var page struct {
			Results []struct {
				ID int64 `json:"id"`
			} `json:"results"`
		}
		if err := json.Unmarshal(raw, &page); err != nil || page.Results == nil {
			return fmt.Errorf("%w: incomplete search page", errProtocol)
		}
		for _, result := range page.Results {
			ids = append(ids, result.ID)
		}
		return nil
	})
	return ids, err
}

// groupIDs lists every group in the account.
func (client *Client) groupIDs(ctx context.Context) ([]int64, error) {
	var ids []int64
	err := client.cursorPages(ctx, "/api/v2/groups.json", url.Values{"page[size]": {"100"}}, func(raw json.RawMessage) error {
		var page struct {
			Groups []struct {
				ID int64 `json:"id"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(raw, &page); err != nil || page.Groups == nil {
			return fmt.Errorf("%w: incomplete group page", errProtocol)
		}
		for _, group := range page.Groups {
			ids = append(ids, group.ID)
		}
		return nil
	})
	return ids, err
}

// cursorPages walks a cursor-paginated endpoint, handing each page's body to visit.
func (client *Client) cursorPages(ctx context.Context, path string, query url.Values, visit func(json.RawMessage) error) error {
	seen := make(map[string]bool)
	for {
		var raw json.RawMessage
		if err := client.get(ctx, path, query, true, &raw); err != nil {
			return err
		}
		var page struct {
			Meta struct {
				HasMore     *bool  `json:"has_more"`
				AfterCursor string `json:"after_cursor"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(raw, &page); err != nil || page.Meta.HasMore == nil {
			return fmt.Errorf("%w: incomplete pagination", errProtocol)
		}
		if err := visit(raw); err != nil {
			return err
		}
		if !*page.Meta.HasMore {
			return nil
		}
		if page.Meta.AfterCursor == "" || seen[page.Meta.AfterCursor] {
			return fmt.Errorf("%w: cursor did not advance", errProtocol)
		}
		seen[page.Meta.AfterCursor] = true
		query.Set("page[after]", page.Meta.AfterCursor)
	}
}

// currentTickets reads the current state of tickets, 100 per request.
// Tickets the API doesn't return (deleted, or unreadable) are absent.
func (client *Client) currentTickets(ctx context.Context, ids []int64) (map[int64]Ticket, error) {
	current := make(map[int64]Ticket, len(ids))
	for chunk := range slices.Chunk(ids, 100) {
		parts := make([]string, len(chunk))
		for i, id := range chunk {
			parts[i] = strconv.FormatInt(id, 10)
		}
		var page struct {
			Tickets []Ticket `json:"tickets"`
		}
		if err := client.get(ctx, "/api/v2/tickets/show_many.json", url.Values{"ids": {strings.Join(parts, ",")}}, true, &page); err != nil {
			return nil, err
		}
		if page.Tickets == nil {
			return nil, fmt.Errorf("%w: incomplete ticket lookup", errProtocol)
		}
		for _, ticket := range page.Tickets {
			if slices.Contains(chunk, ticket.ID) {
				current[ticket.ID] = ticket
			}
		}
	}
	return current, nil
}

func (client *Client) Ticket(ctx context.Context, ticketID int64) (Ticket, []User, error) {
	var response struct {
		Ticket Ticket `json:"ticket"`
		Users  []User `json:"users"`
	}
	err := client.get(ctx, fmt.Sprintf("/api/v2/tickets/%d.json", ticketID), url.Values{"include": {"users"}}, true, &response)
	if err == nil && response.Ticket.ID != ticketID {
		err = fmt.Errorf("%w: ticket identity mismatch", errProtocol)
	}
	return response.Ticket, response.Users, err
}
