package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gisikw/golem/protocol"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	Base  string
	HTTP  *http.Client
	Token string
}

func New(endpoint string) *Client {
	c := &Client{Base: strings.TrimRight(endpoint, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
	if strings.HasPrefix(endpoint, "unix://") {
		path := strings.TrimPrefix(endpoint, "unix://")
		c.Base = "http://unix"
		c.HTTP.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}
	}
	return c
}

func NewWithToken(endpoint, token string) *Client {
	c := New(endpoint)
	c.Token = token
	return c
}

func (c *Client) authorize(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var r io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return e
		}
		r = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.Base+path, r)
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	res, e := c.HTTP.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return fmt.Errorf("agent service: %s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && res.StatusCode != 204 {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}
func (c *Client) Create(ctx context.Context, x protocol.CreateJob) (protocol.Job, error) {
	var j protocol.Job
	e := c.do(ctx, "POST", "/v1/jobs", x, &j)
	return j, e
}
func (c *Client) Get(ctx context.Context, id string) (protocol.Job, error) {
	var j protocol.Job
	e := c.do(ctx, "GET", "/v1/jobs/"+url.PathEscape(id), nil, &j)
	return j, e
}
func (c *Client) List(ctx context.Context, s protocol.State) ([]protocol.Job, error) {
	var j []protocol.Job
	e := c.do(ctx, "GET", "/v1/jobs?state="+url.QueryEscape(string(s)), nil, &j)
	return j, e
}
func (c *Client) Cancel(ctx context.Context, id string) (protocol.Job, error) {
	var j protocol.Job
	e := c.do(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/cancel", struct{}{}, &j)
	return j, e
}
func (c *Client) Reap(ctx context.Context, id string) (protocol.Job, error) {
	var j protocol.Job
	e := c.do(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/reap", struct{}{}, &j)
	return j, e
}
func (c *Client) Answer(ctx context.Context, id string, a protocol.Answer) (protocol.Job, error) {
	var j protocol.Job
	e := c.do(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/answer", a, &j)
	return j, e
}
func (c *Client) Steer(ctx context.Context, id string, steer protocol.Steer) (protocol.Job, error) {
	var j protocol.Job
	e := c.do(ctx, "POST", "/v1/jobs/"+url.PathEscape(id)+"/steer", steer, &j)
	return j, e
}
func (c *Client) Poll(ctx context.Context, k map[string]protocol.State) (protocol.PollResponse, error) {
	var x protocol.PollResponse
	e := c.do(ctx, "POST", "/v1/jobs/poll", protocol.PollRequest{Known: k}, &x)
	return x, e
}
func (c *Client) Capabilities(ctx context.Context) (protocol.Capabilities, error) {
	var x protocol.Capabilities
	e := c.do(ctx, "GET", "/v1/capabilities", nil, &x)
	return x, e
}
func (c *Client) Events(ctx context.Context, b protocol.EventBatch) error {
	return c.do(ctx, "POST", "/v1/events", b, nil)
}
func (c *Client) Artifacts(ctx context.Context, id string) (protocol.ArtifactListing, error) {
	var listing protocol.ArtifactListing
	err := c.do(ctx, "GET", "/v1/jobs/"+url.PathEscape(id)+"/artifacts", nil, &listing)
	return listing, err
}
func (c *Client) FetchArtifact(ctx context.Context, id, path string) (*http.Response, error) {
	parts := strings.Split(path, "/")
	for i := range parts {
		if parts[i] == "" || parts[i] == "." || parts[i] == ".." {
			return nil, fmt.Errorf("malformed artifact path")
		}
		parts[i] = url.PathEscape(parts[i])
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/v1/jobs/"+url.PathEscape(id)+"/artifacts/"+strings.Join(parts, "/"), nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		defer res.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return nil, fmt.Errorf("agent service: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	return res, nil
}

// StreamEvents connects to the resumable SSE feed. Each channel is closed when
// the request ends; terminal transport/decode errors are sent on errs.
func (c *Client) StreamEvents(ctx context.Context, since int64, job string) (<-chan protocol.Event, <-chan error) {
	out := make(chan protocol.Event)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		path := fmt.Sprintf("/v1/events?since=%d&job=%s", since, url.QueryEscape(job))
		req, err := http.NewRequestWithContext(ctx, "GET", c.Base+path, nil)
		if err != nil {
			errs <- err
			return
		}
		c.authorize(req)
		hc := *c.HTTP
		hc.Timeout = 0
		res, err := hc.Do(req)
		if err != nil {
			errs <- err
			return
		}
		defer res.Body.Close()
		if res.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			errs <- fmt.Errorf("agent service: %s: %s", res.Status, strings.TrimSpace(string(b)))
			return
		}
		scan := bufio.NewScanner(res.Body)
		scan.Buffer(make([]byte, 64<<10), 4<<20)
		for scan.Scan() {
			line := scan.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event protocol.Event
			if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				errs <- err
				return
			}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
		if err = scan.Err(); err != nil && ctx.Err() == nil {
			errs <- err
		}
	}()
	return out, errs
}
