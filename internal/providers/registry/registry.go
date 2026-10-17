// Package registry builds every API cloud driver from SPADE_<PROVIDER>_* environment variables.
// A driver without its token stays registered but unconfigured, so deploys to it fail closed.
package registry

import (
	"os"
	"strconv"

	"github.com/munisp/spadeai/internal/providers/cloud"
	"github.com/munisp/spadeai/internal/providers/digitalocean"
	"github.com/munisp/spadeai/internal/providers/hetzner"
	"github.com/munisp/spadeai/internal/providers/linode"
	"github.com/munisp/spadeai/internal/providers/vultr"
)

func FromEnv() []cloud.Driver {
	osID, _ := strconv.Atoi(os.Getenv("SPADE_VULTR_OS_ID"))
	return []cloud.Driver{
		&hetzner.Client{Token: os.Getenv("SPADE_HETZNER_TOKEN"), BaseURL: os.Getenv("SPADE_HETZNER_API"), Location: os.Getenv("SPADE_HETZNER_LOCATION")},
		&digitalocean.Client{Token: os.Getenv("SPADE_DIGITALOCEAN_TOKEN"), BaseURL: os.Getenv("SPADE_DIGITALOCEAN_API"), Region: os.Getenv("SPADE_DIGITALOCEAN_REGION")},
		&vultr.Client{Token: os.Getenv("SPADE_VULTR_TOKEN"), BaseURL: os.Getenv("SPADE_VULTR_API"), Region: os.Getenv("SPADE_VULTR_REGION"), OSID: osID},
		&linode.Client{Token: os.Getenv("SPADE_LINODE_TOKEN"), BaseURL: os.Getenv("SPADE_LINODE_API"), Region: os.Getenv("SPADE_LINODE_REGION")},
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
