// Package digitalocean drives DigitalOcean droplets, tagged spade-svc:<id> / spade-att:<attempt>.
package digitalocean

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/munisp/spadeai/internal/providers/cloud"
)

type Client struct {
	BaseURL string // default https://api.digitalocean.com/v2
	Token   string
	Region  string // default lon1
	HTTP    *http.Client
}

func (c *Client) Name() string     { return "digitalocean" }
func (c *Client) TokenEnv() string { return "SPADE_DIGITALOCEAN_TOKEN" }
func (c *Client) Configured() bool { return c != nil && c.Token != "" }
func (c *Client) InNigeria() bool  { return false }

func (c *Client) api() *cloud.API {
	base := c.BaseURL
	if base == "" {
		base = "https://api.digitalocean.com/v2"
	}
	return &cloud.API{Provider: "digitalocean", TokenEnv: c.TokenEnv(), BaseURL: base, Token: c.Token, HTTP: c.HTTP}
}

type droplet struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Tags     []string `json:"tags"`
	Networks struct {
		V4 []struct {
			IP   string `json:"ip_address"`
			Type string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

func (d droplet) server(attempt string) cloud.Server {
	s := cloud.Server{ID: strconv.FormatInt(d.ID, 10), Name: d.Name, Status: d.Status, Attempt: attempt}
	for _, n := range d.Networks.V4 {
		if n.Type == "public" {
			s.IPv4 = n.IP
		}
	}
	return s
}

func (c *Client) List(ctx context.Context, serviceID string) ([]cloud.Server, error) {
	var res []cloud.Server
	for page := 1; ; page++ {
		var out struct {
			Droplets []droplet `json:"droplets"`
			Links    struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		q := "/droplets?per_page=200&page=" + strconv.Itoa(page) + "&tag_name=" + url.QueryEscape(cloud.ServiceTag(serviceID))
		if _, err := c.api().Do(ctx, http.MethodGet, q, nil, nil, &out); err != nil {
			return nil, err
		}
		for _, d := range out.Droplets {
			if a, ok := cloud.FromTags(d.Tags, serviceID); ok {
				res = append(res, d.server(a))
			}
		}
		if out.Links.Pages.Next == "" || len(out.Droplets) == 0 {
			return res, nil
		}
	}
}

func (c *Client) Create(ctx context.Context, o cloud.CreateOpts) (*cloud.Server, error) {
	region := c.Region
	if region == "" {
		region = "lon1"
	}
	body := map[string]any{
		"name": cloud.ServerName(o), "region": region, "size": o.Size, "image": "ubuntu-24-04-x64",
		"user_data": o.UserData, "tags": []string{cloud.ServiceTag(o.ServiceID), cloud.AttemptTag(o.Attempt), cloud.ManagedTag},
	}
	var out struct {
		Droplet droplet `json:"droplet"`
	}
	if _, err := c.api().Do(ctx, http.MethodPost, "/droplets", nil, body, &out); err != nil {
		return nil, err
	}
	s := out.Droplet.server(o.Attempt)
	return &s, nil
}

func (c *Client) Destroy(ctx context.Context, id string) error {
	code, err := c.api().Do(ctx, http.MethodDelete, "/droplets/"+url.PathEscape(id), nil, nil, nil)
	if code == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.api().Do(ctx, http.MethodGet, "/account", nil, nil, nil)
	return err
}
