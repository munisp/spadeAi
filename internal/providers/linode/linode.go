// Package linode drives Akamai Cloud (Linode) instances, tagged spade-svc:<id> / spade-att:<attempt>.
package linode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/munisp/spadeai/internal/providers/cloud"
)

type Client struct {
	BaseURL string // default https://api.linode.com/v4
	Token   string
	Region  string // default gb-lon (London; supports the Metadata service that delivers cloud-init)
	HTTP    *http.Client
}

func (c *Client) Name() string     { return "linode" }
func (c *Client) TokenEnv() string { return "SPADE_LINODE_TOKEN" }
func (c *Client) Configured() bool { return c != nil && c.Token != "" }

func (c *Client) api() *cloud.API {
	base := c.BaseURL
	if base == "" {
		base = "https://api.linode.com/v4"
	}
	return &cloud.API{Provider: "linode", TokenEnv: c.TokenEnv(), BaseURL: base, Token: c.Token, HTTP: c.HTTP}
}

type instance struct {
	ID     int64    `json:"id"`
	Label  string   `json:"label"`
	Status string   `json:"status"`
	IPv4   []string `json:"ipv4"`
	Tags   []string `json:"tags"`
}

func (i instance) server(attempt string) cloud.Server {
	s := cloud.Server{ID: strconv.FormatInt(i.ID, 10), Name: i.Label, Status: i.Status, Attempt: attempt}
	if len(i.IPv4) > 0 {
		s.IPv4 = i.IPv4[0]
	}
	return s
}

func (c *Client) List(ctx context.Context, serviceID string) ([]cloud.Server, error) {
	filter, _ := json.Marshal(map[string]string{"tags": cloud.ServiceTag(serviceID)})
	var res []cloud.Server
	for page := 1; ; page++ {
		var out struct {
			Data  []instance `json:"data"`
			Pages int        `json:"pages"`
		}
		if _, err := c.api().Do(ctx, http.MethodGet, "/linode/instances?page_size=500&page="+strconv.Itoa(page), map[string]string{"X-Filter": string(filter)}, nil, &out); err != nil {
			return nil, err
		}
		for _, i := range out.Data {
			if a, ok := cloud.FromTags(i.Tags, serviceID); ok {
				res = append(res, i.server(a))
			}
		}
		if page >= out.Pages {
			return res, nil
		}
	}
}

func (c *Client) Create(ctx context.Context, o cloud.CreateOpts) (*cloud.Server, error) {
	region := c.Region
	if region == "" {
		region = "gb-lon"
	}
	// Linode requires a root password; it is random, never stored, and locked by first boot.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	body := map[string]any{
		"region": region, "type": o.Size, "image": "linode/ubuntu24.04", "label": cloud.ServerName(o), "booted": true,
		"root_pass": hex.EncodeToString(raw),
		"metadata":  map[string]string{"user_data": base64.StdEncoding.EncodeToString([]byte(o.UserData))},
		"tags":      []string{cloud.ServiceTag(o.ServiceID), cloud.AttemptTag(o.Attempt), cloud.ManagedTag},
	}
	var out instance
	if _, err := c.api().Do(ctx, http.MethodPost, "/linode/instances", nil, body, &out); err != nil {
		return nil, err
	}
	s := out.server(o.Attempt)
	return &s, nil
}

func (c *Client) Destroy(ctx context.Context, id string) error {
	code, err := c.api().Do(ctx, http.MethodDelete, "/linode/instances/"+url.PathEscape(id), nil, nil, nil)
	if code == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.api().Do(ctx, http.MethodGet, "/profile", nil, nil, nil)
	return err
}
