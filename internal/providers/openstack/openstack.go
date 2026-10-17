// Package openstack drives Nova servers tagged spade-svc:<id> / spade-att:<attempt>, authenticated with a
// Keystone v3 application credential. Nigerian partner clouds commonly run OpenStack, so residency is an
// operator declaration (SPADE_OPENSTACK_IN_NIGERIA), not assumed.
package openstack

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/munisp/spadeai/internal/providers/cloud"
)

type Client struct {
	AuthURL           string // Keystone v3, e.g. https://keystone.example:5000/v3
	AppCredID, Secret string
	Region            string
	Image             string // Ubuntu 24.04 image id
	Network           string // network id; empty asks Nova to auto-allocate
	InNigeriaDeclared bool
	HTTP              *http.Client
}

func (c *Client) Name() string { return "openstack" }
func (c *Client) TokenEnv() string {
	return "SPADE_OPENSTACK_AUTH_URL/SPADE_OPENSTACK_APP_CRED_ID/SPADE_OPENSTACK_APP_CRED_SECRET/SPADE_OPENSTACK_IMAGE"
}
func (c *Client) Configured() bool {
	return c != nil && c.AuthURL != "" && c.AppCredID != "" && c.Secret != "" && c.Image != ""
}
func (c *Client) InNigeria() bool { return c.InNigeriaDeclared }

// compute authenticates per call; tags at create need microversion 2.52.
func (c *Client) compute(ctx context.Context) (*gophercloud.ServiceClient, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("openstack: %s not configured", c.TokenEnv())
	}
	ao := gophercloud.AuthOptions{IdentityEndpoint: c.AuthURL, ApplicationCredentialID: c.AppCredID, ApplicationCredentialSecret: c.Secret}
	pc, err := openstack.NewClient(ao.IdentityEndpoint)
	if err != nil {
		return nil, fmt.Errorf("openstack: %w", err)
	}
	if c.HTTP != nil {
		pc.HTTPClient = *c.HTTP
	} else {
		pc.HTTPClient = http.Client{Timeout: 30 * time.Second}
	}
	if err := openstack.Authenticate(ctx, pc, ao); err != nil {
		return nil, fmt.Errorf("openstack: auth: %w", err)
	}
	sc, err := openstack.NewComputeV2(pc, gophercloud.EndpointOpts{Region: c.Region})
	if err != nil {
		return nil, fmt.Errorf("openstack: %w", err)
	}
	sc.Microversion = "2.52"
	return sc, nil
}

func ipv4(addrs map[string]any) string {
	var fixed string
	for _, v := range addrs {
		list, _ := v.([]any)
		for _, a := range list {
			m, _ := a.(map[string]any)
			if ver, _ := m["version"].(float64); ver != 4 {
				continue
			}
			ip, _ := m["addr"].(string)
			if m["OS-EXT-IPS:type"] == "floating" {
				return ip
			}
			if fixed == "" {
				fixed = ip
			}
		}
	}
	return fixed
}

func (c *Client) List(ctx context.Context, serviceID string) ([]cloud.Server, error) {
	sc, err := c.compute(ctx)
	if err != nil {
		return nil, err
	}
	var res []cloud.Server
	err = servers.List(sc, servers.ListOpts{Tags: cloud.ServiceTag(serviceID)}).EachPage(ctx, func(_ context.Context, p pagination.Page) (bool, error) {
		ss, err := servers.ExtractServers(p)
		if err != nil {
			return false, err
		}
		for _, s := range ss {
			if s.Tags == nil {
				continue
			}
			if a, ok := cloud.FromTags(*s.Tags, serviceID); ok {
				res = append(res, cloud.Server{ID: s.ID, Name: s.Name, Status: s.Status, IPv4: ipv4(s.Addresses), Attempt: a})
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("openstack: list: %w", err)
	}
	return res, nil
}

// flavor resolves a size code (the flavor name) to its id.
func flavor(ctx context.Context, sc *gophercloud.ServiceClient, name string) (string, error) {
	var id string
	err := flavors.ListDetail(sc, flavors.ListOpts{}).EachPage(ctx, func(_ context.Context, p pagination.Page) (bool, error) {
		fs, err := flavors.ExtractFlavors(p)
		if err != nil {
			return false, err
		}
		for _, f := range fs {
			if f.Name == name {
				id = f.ID
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("openstack: flavors: %w", err)
	}
	if id == "" {
		return "", fmt.Errorf("openstack: flavor %q not found", name)
	}
	return id, nil
}

func (c *Client) Create(ctx context.Context, o cloud.CreateOpts) (*cloud.Server, error) {
	sc, err := c.compute(ctx)
	if err != nil {
		return nil, err
	}
	fid, err := flavor(ctx, sc, o.Size)
	if err != nil {
		return nil, err
	}
	var nets any = "auto"
	if c.Network != "" {
		nets = []servers.Network{{UUID: c.Network}}
	}
	s, err := servers.Create(ctx, sc, servers.CreateOpts{
		Name: cloud.ServerName(o), ImageRef: c.Image, FlavorRef: fid, UserData: []byte(o.UserData), Networks: nets,
		Tags:     []string{cloud.ServiceTag(o.ServiceID), cloud.AttemptTag(o.Attempt), cloud.ManagedTag},
		Metadata: map[string]string{"spade-service": o.ServiceID, "spade-attempt": o.Attempt},
	}, nil).Extract()
	if err != nil {
		return nil, fmt.Errorf("openstack: create: %w", err)
	}
	return &cloud.Server{ID: s.ID, Name: cloud.ServerName(o), Status: s.Status, IPv4: ipv4(s.Addresses), Attempt: o.Attempt}, nil
}

func (c *Client) Destroy(ctx context.Context, id string) error {
	sc, err := c.compute(ctx)
	if err != nil {
		return err
	}
	if err := servers.Delete(ctx, sc, id).ExtractErr(); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return fmt.Errorf("openstack: delete %s: %w", id, err)
	}
	return nil
}

// Ping authenticates and reads one page of flavors.
func (c *Client) Ping(ctx context.Context) error {
	sc, err := c.compute(ctx)
	if err != nil {
		return err
	}
	if _, err := flavors.ListDetail(sc, flavors.ListOpts{Limit: 1}).AllPages(ctx); err != nil {
		return fmt.Errorf("openstack: %w", err)
	}
	return nil
}
