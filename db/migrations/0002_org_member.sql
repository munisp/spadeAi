CREATE TABLE org_member (
  user_id    uuid NOT NULL REFERENCES app_user(id),
  org_id     uuid NOT NULL REFERENCES organization(id),
  role       text NOT NULL CHECK (role IN ('owner','member')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, org_id)
);
