// Package mqadmin is a tiny read-only client for the RabbitMQ HTTP management API,
// used to discover which worker-pool regions currently have a live worker (an
// active consumer on checks.jobs.<region>).
package mqadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	jobsQueuePrefix   = "checks.jobs."
	jobsV2QueuePrefix = "checks.jobs.v2."
	jobsV3QueuePrefix = "checks.jobs.v3."
)

// Client queries the RabbitMQ management API.
type Client struct {
	base string // e.g. http://host:15672
	user string
	pass string
	http *http.Client
}

// New builds a client from a management base URL that may embed credentials
// (http://user:pass@host:15672).
func New(managementURL string) (*Client, error) {
	u, err := url.Parse(managementURL)
	if err != nil {
		return nil, fmt.Errorf("mqadmin: parse management url: %w", err)
	}
	c := &Client{http: &http.Client{Timeout: 5 * time.Second}}
	if u.User != nil {
		c.user = u.User.Username()
		c.pass, _ = u.User.Password()
	}
	u.User = nil
	c.base = strings.TrimRight(u.String(), "/")
	return c, nil
}

// FromAMQP derives a management client from an amqp(s) URL by swapping the scheme
// to http(s) and the port to the management port (5672→15672, 5671→15671).
func FromAMQP(amqpURL string) (*Client, error) {
	u, err := url.Parse(amqpURL)
	if err != nil {
		return nil, fmt.Errorf("mqadmin: parse amqp url: %w", err)
	}
	scheme, mgmtPort := "http", "15672"
	if u.Scheme == "amqps" {
		scheme, mgmtPort = "https", "15671"
	}
	host := u.Hostname()
	if host == "" {
		host = "localhost"
	}
	mgmt := &url.URL{Scheme: scheme, Host: host + ":" + mgmtPort, User: u.User}
	return New(mgmt.String())
}

type queue struct {
	Name      string `json:"name"`
	Consumers int    `json:"consumers"`
}

// LiveJobRegions returns the set of regions that have at least one active consumer
// on their checks.jobs.<region> queue (i.e. a live worker).
func (c *Client) LiveJobRegions(ctx context.Context) (map[string]bool, error) {
	queues, err := c.liveQueues(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, q := range queues {
		if q.Consumers > 0 && strings.HasPrefix(q.Name, jobsQueuePrefix) &&
			!strings.HasPrefix(q.Name, jobsV2QueuePrefix) && !strings.HasPrefix(q.Name, jobsV3QueuePrefix) {
			live[strings.TrimPrefix(q.Name, jobsQueuePrefix)] = true
		}
	}
	return live, nil
}

// LiveCredentialJobRegions returns regions with at least one consumer on the physically
// isolated v2 queue. This is the runtime half of the envelope wire barrier: existence of a
// legacy checks.jobs.<region> consumer never authorizes credential dispatch.
func (c *Client) LiveCredentialJobRegions(ctx context.Context) (map[string]bool, error) {
	queues, err := c.liveQueues(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, q := range queues {
		if q.Consumers > 0 && strings.HasPrefix(q.Name, jobsV2QueuePrefix) {
			live[strings.TrimPrefix(q.Name, jobsV2QueuePrefix)] = true
		}
	}
	return live, nil
}

// LiveCredentialV3JobRegions is the same existential check one generation further on: a
// region qualifies only when something is consuming the generation-3 carrier. A consumer
// on the older envelope queue is not evidence of readiness for the newer one — that is the
// whole reason capability is generational rather than boolean (§4.7, D-0160).
func (c *Client) LiveCredentialV3JobRegions(ctx context.Context) (map[string]bool, error) {
	queues, err := c.liveQueues(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, q := range queues {
		if q.Consumers > 0 && strings.HasPrefix(q.Name, jobsV3QueuePrefix) {
			live[strings.TrimPrefix(q.Name, jobsV3QueuePrefix)] = true
		}
	}
	return live, nil
}

func (c *Client) liveQueues(ctx context.Context) ([]queue, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/queues?columns=name,consumers", nil)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mqadmin: get queues: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mqadmin: queues status %d", resp.StatusCode)
	}
	var queues []queue
	if err := json.NewDecoder(resp.Body).Decode(&queues); err != nil {
		return nil, fmt.Errorf("mqadmin: decode queues: %w", err)
	}
	return queues, nil
}
