package client

import (
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
	Base string
	HTTP *http.Client
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
