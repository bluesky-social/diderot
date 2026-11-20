package zendesk

import (
	"net/http"
	"strconv"
	"time"
)

func (client *Client) observeRateLimit(response *http.Response) {
	var delay time.Duration
	if response.StatusCode == http.StatusTooManyRequests {
		delay = max(time.Second, retryDelay(response.Header.Get("Retry-After")))
	}
	remaining := response.Header.Get("ratelimit-remaining")
	if remaining == "" {
		remaining = response.Header.Get("X-Rate-Limit-Remaining")
	}
	if remaining == "0" {
		if seconds, err := strconv.ParseInt(response.Header.Get("ratelimit-reset"), 10, 32); err == nil && seconds > 0 {
			delay = max(delay, time.Duration(seconds)*time.Second)
		}
	}
	if delay > 0 {
		client.limiter.PauseUntil(time.Now().Add(delay))
	}
}

func (client *Client) send(request *http.Request, transport *http.Client) (*http.Response, error) {
	if err := client.limiter.Wait(request.Context()); err != nil {
		return nil, err
	}
	response, err := transport.Do(request)
	if response != nil {
		location := request.URL
		if response.Request != nil {
			location = response.Request.URL
		}
		if location.Scheme+"://"+location.Host == client.origin {
			client.observeRateLimit(response)
		}
	}
	return response, err
}
