-- AWS EC2, OpenStack and Proxmox on the shared adapter. in_nigeria for openstack/proxmox is set at API
-- start from SPADE_OPENSTACK_IN_NIGERIA / SPADE_PROXMOX_IN_NIGERIA; AWS has no Nigerian region.
INSERT INTO provider (id, kind, name) VALUES
  ('aws','api','Amazon Web Services (EC2)'),
  ('openstack','api','OpenStack (Nova)'),
  ('proxmox','api','Proxmox VE')
ON CONFLICT DO NOTHING;
-- OpenStack codes are flavor names and must exist on the target cloud; Proxmox codes end in <cores>C-<GiB>G.
INSERT INTO size (id, provider_id, code, vcpu, ram_mb, disk_gb) VALUES
  ('aws:t3.small','aws','t3.small',2,2048,30),
  ('aws:t3.medium','aws','t3.medium',2,4096,30),
  ('aws:t3.large','aws','t3.large',2,8192,30),
  ('openstack:m1.small','openstack','m1.small',1,2048,20),
  ('openstack:m1.medium','openstack','m1.medium',2,4096,40),
  ('openstack:m1.large','openstack','m1.large',4,8192,80),
  ('proxmox:SMALL-1C-2G','proxmox','SMALL-1C-2G',1,2048,32),
  ('proxmox:MEDIUM-2C-4G','proxmox','MEDIUM-2C-4G',2,4096,32),
  ('proxmox:LARGE-4C-8G','proxmox','LARGE-4C-8G',4,8192,32)
ON CONFLICT DO NOTHING;
