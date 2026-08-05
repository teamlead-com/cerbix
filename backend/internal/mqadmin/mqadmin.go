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

const jobsQueuePrefix = "checks.jobs."

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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mqadmin: queues status %d", resp.StatusCode)
	}
	var queues []queue
	if err := json.NewDecoder(resp.Body).Decode(&queues); err != nil {
		return nil, fmt.Errorf("mqadmin: decode queues: %w", err)
	}
	live := map[string]bool{}
	for _, q := range queues {
		if q.Consumers > 0 && strings.HasPrefix(q.Name, jobsQueuePrefix) {
			live[strings.TrimPrefix(q.Name, jobsQueuePrefix)] = true
		}
	}
	return live, nil
}
