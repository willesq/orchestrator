package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Slack's Web API.
const DefaultBaseURL = "https://slack.com/api/"

const (
	// maxBody caps one API response read into memory.
	maxBody = 16 << 20
	// maxPages caps one listing's pages, so a server that never stops
	// paging cannot keep a run going for ever.
	maxPages = 5000
	// maxRetries is how often a rate-limited call is tried again.
	maxRetries = 5
	// maxRetryWait caps the wait Slack's Retry-After asks for.
	maxRetryWait = time.Minute
	// maxListing caps the bytes one listing reads over all its pages, so
	// a conversation too large for memory stops the run instead.
	maxListing = 256 << 20
)

// client calls Slack's Web API with one token. It lives only in the
// connector process: the token is sent in the Authorization header, never
// in a URL, and no error it returns includes it.
type client struct {
	base  string
	token string
	http  *http.Client
	// sleep waits d or until ctx ends (tests make it instant).
	sleep func(ctx context.Context, d time.Duration) error
}

// APIError is an error Slack reported for a call (ok: false).
type APIError struct {
	Method string
	// Code is Slack's error code, e.g. invalid_auth or channel_not_found.
	Code string
}

func (e *APIError) Error() string { return "slack " + e.Method + ": " + e.Code }

// codePattern is the shape of a Slack error code. Anything else a server
// puts there is not repeated in an error.
var codePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// call posts params to method and decodes the response into out.
func (c *client) call(ctx context.Context, method string, params url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+method, strings.NewReader(params.Encode()))
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			if err := c.sleep(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return fmt.Errorf("slack %s: %w", method, err)
			}
			continue
		}
		switch {
		case err != nil:
			return fmt.Errorf("slack %s: read the response: %w", method, err)
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("slack %s: HTTP %d", method, resp.StatusCode)
		case len(body) > maxBody:
			return fmt.Errorf("slack %s: the response is larger than %d bytes", method, maxBody)
		}
		var env struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return fmt.Errorf("slack %s: the response is not JSON", method)
		}
		if !env.OK {
			code := env.Error
			if !codePattern.MatchString(code) {
				code = "unknown_error"
			}
			return &APIError{Method: method, Code: code}
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("slack %s: unexpected response: %w", method, err)
		}
		return nil
	}
}

// retryAfter is how long Slack's Retry-After header asks to wait, at least
// a second and at most maxRetryWait.
func retryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n < 1 {
		return time.Second
	}
	return min(time.Duration(n)*time.Second, maxRetryWait)
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// identity is who the token belongs to (auth.test).
type identity struct {
	Team   string `json:"team"`
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
}

// conversation is a channel, DM or group DM (conversations.list and
// conversations.info).
type conversation struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsIM       bool   `json:"is_im"`
	IsMPIM     bool   `json:"is_mpim"`
	IsPrivate  bool   `json:"is_private"`
	IsArchived bool   `json:"is_archived"`
	// User is the other member of a DM.
	User string `json:"user"`
}

// message is one Slack message (conversations.history and
// conversations.replies).
type message struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	ReplyCount  int    `json:"reply_count"`
	LatestReply string `json:"latest_reply"`
}

// cursorPage is the cursor every paged response carries.
type cursorPage struct {
	Meta struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

func (c *client) authTest(ctx context.Context) (identity, error) {
	var id identity
	err := c.call(ctx, "auth.test", url.Values{}, &id)
	return id, err
}

func (c *client) info(ctx context.Context, channel string) (conversation, error) {
	var r struct {
		Channel conversation `json:"channel"`
	}
	err := c.call(ctx, "conversations.info", url.Values{"channel": {channel}}, &r)
	return r.Channel, err
}

// list returns every conversation of the given types the user is in.
func (c *client) list(ctx context.Context, types string) ([]conversation, error) {
	var all []conversation
	err := c.pages(ctx, "conversations.list", url.Values{"types": {types}, "exclude_archived": {"true"}}, func(body json.RawMessage) error {
		var r struct {
			Channels []conversation `json:"channels"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		all = append(all, r.Channels...)
		return nil
	})
	return all, err
}

// history returns channel's top-level messages after oldest ("" for all).
func (c *client) history(ctx context.Context, channel, oldest string) ([]message, error) {
	params := url.Values{"channel": {channel}}
	if oldest != "" {
		params.Set("oldest", oldest)
	}
	return c.messages(ctx, "conversations.history", params)
}

// replies returns a thread's messages, its parent first.
func (c *client) replies(ctx context.Context, channel, ts string) ([]message, error) {
	return c.messages(ctx, "conversations.replies", url.Values{"channel": {channel}, "ts": {ts}})
}

func (c *client) messages(ctx context.Context, method string, params url.Values) ([]message, error) {
	var all []message
	err := c.pages(ctx, method, params, func(body json.RawMessage) error {
		var r struct {
			Messages []message `json:"messages"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		all = append(all, r.Messages...)
		return nil
	})
	return all, err
}

// errCursorLoop stops a listing whose server hands back a cursor it gave
// before.
var errCursorLoop = errors.New("the server repeated a page cursor")

// pages calls method page by page, handing each response to page.
func (c *client) pages(ctx context.Context, method string, params url.Values, page func(json.RawMessage) error) error {
	seen := map[string]bool{}
	params.Set("limit", "200")
	total := 0
	for n := 0; n < maxPages; n++ {
		var body json.RawMessage
		if err := c.call(ctx, method, params, &body); err != nil {
			return err
		}
		if total += len(body); total > maxListing {
			return fmt.Errorf("slack %s: more than %d bytes; read less history", method, maxListing)
		}
		if err := page(body); err != nil {
			return fmt.Errorf("slack %s: unexpected response: %w", method, err)
		}
		var cur cursorPage
		if err := json.Unmarshal(body, &cur); err != nil {
			return fmt.Errorf("slack %s: unexpected response: %w", method, err)
		}
		next := cur.Meta.NextCursor
		if next == "" {
			return nil
		}
		if seen[next] {
			return fmt.Errorf("slack %s: %w", method, errCursorLoop)
		}
		seen[next] = true
		params.Set("cursor", next)
	}
	return fmt.Errorf("slack %s: more than %d pages", method, maxPages)
}
