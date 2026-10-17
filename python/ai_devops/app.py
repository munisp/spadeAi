"""AI DevOps HTTP service. Reads org policy from Postgres, routes the chat, records the session for audit/billing."""
from __future__ import annotations

import json
import os

import httpx
import psycopg
from fastapi import FastAPI, Header, HTTPException
from pydantic import BaseModel

from .router import Policy, Router, Unavailable, backends_from_env

DSN = os.environ.get("SPADE_DATABASE_URL", "postgresql://spade:spade-local-dev@localhost:5433/spadeai")
CORE = os.environ.get("CORE_API_URL", "http://localhost:8080")
SYSTEM = ("You are SpadeAi DevOps. Diagnose managed open-source services from the provided context. "
          "Propose actions; never claim an action was executed.")

app = FastAPI(title="spade-ai-devops")
router = Router(backends_from_env())


class Ask(BaseModel):
    org_id: str
    question: str
    context: dict | None = None


@app.get("/healthz")
def healthz():
    if not any(b.configured for b in router.backends):
        raise HTTPException(503, "no LLM backend configured")
    return {"backends": [{"name": b.name, "kind": b.kind, "configured": b.configured} for b in router.backends]}


@app.post("/v1/ai-sessions")
def ask(body: Ask, authorization: str = Header(default="")):
    # Membership check via the core API using the caller's token.
    r = httpx.get(f"{CORE}/v1/orgs/{body.org_id}/wallet", headers={"Authorization": authorization}, timeout=5)
    if r.status_code != 200:
        raise HTTPException(403, "not authorised for this organization")
    with psycopg.connect(DSN) as conn:
        row = conn.execute("SELECT ai_policy, residency_locked FROM organization WHERE id=%s", (body.org_id,)).fetchone()
        if not row:
            raise HTTPException(404, "organization not found")
        policy = Policy(row[0], row[1])
        msgs = [{"role": "system", "content": SYSTEM},
                {"role": "user", "content": body.question + ("\n\nContext:\n" + json.dumps(body.context) if body.context else "")}]
        try:
            res = router.chat(policy, msgs)
        except Unavailable as e:
            raise HTTPException(503, str(e)) from e
        conn.execute(
            """INSERT INTO ai_session(org_id, policy, backend, backend_kind, model, prompt_tokens, completion_tokens, attempts)
               VALUES (%s,%s,%s,%s,%s,%s,%s,%s)""",
            (body.org_id, policy.effective(), res.backend, res.kind, res.model, res.prompt_tokens, res.completion_tokens, json.dumps(res.attempts)))
    return {"answer": res.content, "backend": res.backend, "backend_kind": res.kind, "model": res.model, "policy": policy.effective()}
