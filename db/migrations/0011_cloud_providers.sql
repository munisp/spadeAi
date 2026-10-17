-- More API cloud providers on the shared adapter (internal/providers/cloud). Residency is data, not code:
-- residency-locked orgs may only deploy to providers with in_nigeria (BYOVM is the customer's own host).
ALTER TABLE provider ADD COLUMN IF NOT EXISTS in_nigeria boolean NOT NULL DEFAULT false;
UPDATE provider SET in_nigeria = true WHERE id = 'byovm';
INSERT INTO provider (id, kind, name) VALUES
  ('digitalocean','api','DigitalOcean'),
  ('vultr','api','Vultr'),
  ('linode','api','Akamai Cloud (Linode)')
ON CONFLICT DO NOTHING;
INSERT INTO size (id, provider_id, code, vcpu, ram_mb, disk_gb) VALUES
  ('digitalocean:s-1vcpu-2gb','digitalocean','s-1vcpu-2gb',1,2048,50),
  ('digitalocean:s-2vcpu-4gb','digitalocean','s-2vcpu-4gb',2,4096,80),
  ('digitalocean:s-4vcpu-8gb','digitalocean','s-4vcpu-8gb',4,8192,160),
  ('vultr:vc2-1c-2gb','vultr','vc2-1c-2gb',1,2048,55),
  ('vultr:vc2-2c-4gb','vultr','vc2-2c-4gb',2,4096,80),
  ('vultr:vc2-4c-8gb','vultr','vc2-4c-8gb',4,8192,160),
  ('linode:g6-standard-1','linode','g6-standard-1',1,2048,50),
  ('linode:g6-standard-2','linode','g6-standard-2',2,4096,80),
  ('linode:g6-standard-4','linode','g6-standard-4',4,8192,160)
ON CONFLICT DO NOTHING;
