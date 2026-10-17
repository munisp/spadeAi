-- Hetzner Cloud as the first API provider. Sizes map 1:1 to Hetzner server types; management is billed
-- per vCPU/RAM/disk from the price book like every other size (infrastructure is on the customer's own
-- Hetzner account/token for now).
INSERT INTO provider (id, kind, name) VALUES ('hetzner','api','Hetzner Cloud') ON CONFLICT DO NOTHING;
INSERT INTO size (id, provider_id, code, vcpu, ram_mb, disk_gb) VALUES
  ('hetzner:CX22','hetzner','CX22',2,4096,40),
  ('hetzner:CX32','hetzner','CX32',4,8192,80),
  ('hetzner:CX42','hetzner','CX42',8,16384,160)
ON CONFLICT DO NOTHING;
