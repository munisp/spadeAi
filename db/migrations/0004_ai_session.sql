CREATE TABLE ai_session (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id            uuid NOT NULL REFERENCES organization(id),
  policy            text NOT NULL,
  backend           text NOT NULL,
  backend_kind      text NOT NULL CHECK (backend_kind IN ('self_hosted','external')),
  model             text NOT NULL,
  prompt_tokens     int NOT NULL,
  completion_tokens int NOT NULL,
  attempts          jsonb NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now()
);
