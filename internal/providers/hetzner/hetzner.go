// Package hetzner drives Hetzner Cloud. Servers carry spade-service/spade-attempt labels so the shared
// cloud adapter can reconcile them.
package hetzner

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/munisp/spadeai/internal/providers/cloud"
)

type Client struct {
	BaseURL  string // default https://api.hetzner.cloud/v1
	Token    string
	Location string // default fsn1
	HTTP     *http.Client
}

func (c *Client) Name() string     { return "hetzner" }
func (c *Client) TokenEnv() string { return "SPADE_HETZNER_TOKEN" }
func (c *Client) Configured() bool { return c != nil && c.Token != "" }

func (c *Client) api() *cloud.API {
	base := c.BaseURL
	if base == "" {
		base = "https://api.hetzner.cloud/v1"
	}
	return &cloud.API{Provider: "hetzner", TokenEnv: c.TokenEnv(), BaseURL: base, Token: c.Token, HTTP: c.HTTP}
}

type apiServer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

func (a apiServer) server() cloud.Server {
	return cloud.Server{ID: strconv.FormatInt(a.ID, 10), Name: a.Name, Status: a.Status, IPv4: a.PublicNet.IPv4.IP, Attempt: a.Labels["spade-attempt"]}
}

func Label(serviceID string) string { return "spade-service=" + serviceID }

func (c *Client) List(ctx context.Context, serviceID string) ([]cloud.Server, error) {
	var out struct {
		Servers []apiServer `json:"servers"`
	}
	if _, err := c.api().Do(ctx, http.MethodGet, "/servers?per_page=50&label_selector="+url.QueryEscape(Label(serviceID)), nil, nil, &out); err != nil {
		return nil, err
	}
	var res []cloud.Server
	for _, s := range out.Servers {
		if s.Labels["spade-service"] == serviceID {
			res = append(res, s.server())
		}
	}
	return res, nil
}

func (c *Client) Create(ctx context.Context, o cloud.CreateOpts) (*cloud.Server, error) {
	loc := c.Location
	if loc == "" {
		loc = "fsn1"
	}
	body := map[string]any{
		"name": cloud.ServerName(o), "server_type": strings.ToLower(o.Size), "image": "ubuntu-24.04", "location": loc,
		"labels":    map[string]string{"spade-service": o.ServiceID, "spade-attempt": o.Attempt, "spade-managed": "true"},
		"user_data": o.UserData, "start_after_create": true,
	}
	var out struct {
		Server apiServer `json:"server"`
	}
	if _, err := c.api().Do(ctx, http.MethodPost, "/servers", nil, body, &out); err != nil {
		return nil, err
	}
	s := out.Server.server()
	return &s, nil
}

func (c *Client) Destroy(ctx context.Context, id string) error {
	code, err := c.api().Do(ctx, http.MethodDelete, "/servers/"+url.PathEscape(id), nil, nil, nil)
	if code == http.StatusNotFound {
		return nil
	}
	return err
}

// Ping verifies the token against a cheap read-only endpoint.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.api().Do(ctx, http.MethodGet, "/locations", nil, nil, nil)
	return err
}
