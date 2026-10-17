// Package vultr drives Vultr cloud compute instances, tagged spade-svc:<id> / spade-att:<attempt>.
package vultr

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"

	"github.com/munisp/spadeai/internal/providers/cloud"
)

// Ubuntu 24.04 LTS x64 in Vultr's OS catalogue (GET /v2/os); override with OSID if it changes.
const ubuntu2404 = 2284

type Client struct {
	BaseURL string // default https://api.vultr.com/v2
	Token   string
	Region  string // default jnb (Johannesburg, the closest Vultr region to Nigeria)
	OSID    int
	HTTP    *http.Client
}

func (c *Client) Name() string     { return "vultr" }
func (c *Client) TokenEnv() string { return "SPADE_VULTR_TOKEN" }
func (c *Client) Configured() bool { return c != nil && c.Token != "" }

func (c *Client) api() *cloud.API {
	base := c.BaseURL
	if base == "" {
		base = "https://api.vultr.com/v2"
	}
	return &cloud.API{Provider: "vultr", TokenEnv: c.TokenEnv(), BaseURL: base, Token: c.Token, HTTP: c.HTTP}
}

type instance struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	Status string   `json:"status"`
	MainIP string   `json:"main_ip"`
	Tags   []string `json:"tags"`
}

func (i instance) server(attempt string) cloud.Server {
	ip := i.MainIP
	if ip == "0.0.0.0" {
		ip = ""
	}
	return cloud.Server{ID: i.ID, Name: i.Label, Status: i.Status, IPv4: ip, Attempt: attempt}
}

func (c *Client) List(ctx context.Context, serviceID string) ([]cloud.Server, error) {
	var res []cloud.Server
	cursor := ""
	for {
		var out struct {
			Instances []instance `json:"instances"`
			Meta      struct {
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			} `json:"meta"`
		}
		q := "/instances?per_page=500&tag=" + url.QueryEscape(cloud.ServiceTag(serviceID))
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		if _, err := c.api().Do(ctx, http.MethodGet, q, nil, nil, &out); err != nil {
			return nil, err
		}
		for _, i := range out.Instances {
			if a, ok := cloud.FromTags(i.Tags, serviceID); ok {
				res = append(res, i.server(a))
			}
		}
		if cursor = out.Meta.Links.Next; cursor == "" {
			return res, nil
		}
	}
}

func (c *Client) Create(ctx context.Context, o cloud.CreateOpts) (*cloud.Server, error) {
	region, osID := c.Region, c.OSID
	if region == "" {
		region = "jnb"
	}
	if osID == 0 {
		osID = ubuntu2404
	}
	name := cloud.ServerName(o)
	body := map[string]any{
		"region": region, "plan": o.Size, "os_id": osID, "label": name, "hostname": name,
		"user_data": base64.StdEncoding.EncodeToString([]byte(o.UserData)),
		"tags":      []string{cloud.ServiceTag(o.ServiceID), cloud.AttemptTag(o.Attempt), cloud.ManagedTag},
	}
	var out struct {
		Instance instance `json:"instance"`
	}
	if _, err := c.api().Do(ctx, http.MethodPost, "/instances", nil, body, &out); err != nil {
		return nil, err
	}
	s := out.Instance.server(o.Attempt)
	return &s, nil
}

func (c *Client) Destroy(ctx context.Context, id string) error {
	code, err := c.api().Do(ctx, http.MethodDelete, "/instances/"+url.PathEscape(id), nil, nil, nil)
	if code == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.api().Do(ctx, http.MethodGet, "/account", nil, nil, nil)
	return err
}
