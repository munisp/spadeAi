// Package registry builds every API cloud driver from SPADE_<PROVIDER>_* environment variables.
// A driver without its token stays registered but unconfigured, so deploys to it fail closed.
package registry

import (
	"os"
	"strconv"
	"strings"

	"github.com/munisp/spadeai/internal/providers/aws"
	"github.com/munisp/spadeai/internal/providers/cloud"
	"github.com/munisp/spadeai/internal/providers/digitalocean"
	"github.com/munisp/spadeai/internal/providers/hetzner"
	"github.com/munisp/spadeai/internal/providers/linode"
	"github.com/munisp/spadeai/internal/providers/openstack"
	"github.com/munisp/spadeai/internal/providers/proxmox"
	"github.com/munisp/spadeai/internal/providers/vultr"
)

// FromEnv only reads SPADE_* variables; ambient AWS_* / OS_* credentials are never used.
// SPADE_OPENSTACK_IN_NIGERIA / SPADE_PROXMOX_IN_NIGERIA are the operator's declaration that the
// cloud is hosted in Nigeria, which lets residency-locked organisations deploy to it.
func FromEnv() []cloud.Driver {
	osID, _ := strconv.Atoi(os.Getenv("SPADE_VULTR_OS_ID"))
	tmpl, _ := strconv.Atoi(os.Getenv("SPADE_PROXMOX_TEMPLATE"))
	return []cloud.Driver{
		&hetzner.Client{Token: os.Getenv("SPADE_HETZNER_TOKEN"), BaseURL: os.Getenv("SPADE_HETZNER_API"), Location: os.Getenv("SPADE_HETZNER_LOCATION")},
		&digitalocean.Client{Token: os.Getenv("SPADE_DIGITALOCEAN_TOKEN"), BaseURL: os.Getenv("SPADE_DIGITALOCEAN_API"), Region: os.Getenv("SPADE_DIGITALOCEAN_REGION")},
		&vultr.Client{Token: os.Getenv("SPADE_VULTR_TOKEN"), BaseURL: os.Getenv("SPADE_VULTR_API"), Region: os.Getenv("SPADE_VULTR_REGION"), OSID: osID},
		&linode.Client{Token: os.Getenv("SPADE_LINODE_TOKEN"), BaseURL: os.Getenv("SPADE_LINODE_API"), Region: os.Getenv("SPADE_LINODE_REGION")},
		&aws.Client{AccessKeyID: os.Getenv("SPADE_AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("SPADE_AWS_SECRET_ACCESS_KEY"),
			Region: os.Getenv("SPADE_AWS_REGION"), AMI: os.Getenv("SPADE_AWS_AMI"), SubnetID: os.Getenv("SPADE_AWS_SUBNET_ID"),
			SecurityGroupIDs: list(os.Getenv("SPADE_AWS_SECURITY_GROUP_IDS")), BaseURL: os.Getenv("SPADE_AWS_API")},
		&openstack.Client{AuthURL: os.Getenv("SPADE_OPENSTACK_AUTH_URL"), AppCredID: os.Getenv("SPADE_OPENSTACK_APP_CRED_ID"),
			Secret: os.Getenv("SPADE_OPENSTACK_APP_CRED_SECRET"), Region: os.Getenv("SPADE_OPENSTACK_REGION"), Image: os.Getenv("SPADE_OPENSTACK_IMAGE"),
			Network: os.Getenv("SPADE_OPENSTACK_NETWORK"), InNigeriaDeclared: os.Getenv("SPADE_OPENSTACK_IN_NIGERIA") == "1"},
		&proxmox.Client{BaseURL: os.Getenv("SPADE_PROXMOX_API"), Token: os.Getenv("SPADE_PROXMOX_TOKEN"), Node: os.Getenv("SPADE_PROXMOX_NODE"),
			Template: tmpl, FullClone: os.Getenv("SPADE_PROXMOX_FULL_CLONE") == "1", Storage: os.Getenv("SPADE_PROXMOX_STORAGE"),
			Pool: os.Getenv("SPADE_PROXMOX_POOL"), CAFile: os.Getenv("SPADE_PROXMOX_CA_FILE"), InNigeriaDeclared: os.Getenv("SPADE_PROXMOX_IN_NIGERIA") == "1"},
	}
}

// ByName indexes drivers by provider id.
func ByName(ds []cloud.Driver) map[string]cloud.Driver {
	m := make(map[string]cloud.Driver, len(ds))
	for _, d := range ds {
		m[d.Name()] = d
	}
	return m
}

func list(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
