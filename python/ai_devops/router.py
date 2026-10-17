"""LLM router for AI DevOps. Routes strictly within the organization's ai_policy (fail closed).

- self_hosted: only self-hosted backends (vLLM, OpenAI-compatible) — never external.
- external:    only allow-listed external providers.
- hybrid:      self-hosted first, then allow-listed external as fallback.
Residency-locked orgs are forced to self_hosted regardless of stored policy.
"""
from __future__ import annotations

import os
import time
from dataclasses import dataclass, field

import httpx


class Unavailable(Exception):
    """No backend permitted by policy could serve the request."""


@dataclass(frozen=True)
class Backend:
    name: str
    kind: str  # "self_hosted" | "external"
    base_url: str
    model: str
    api_key: str = ""

    @property
    def configured(self) -> bool:
        return bool(self.base_url and self.model) and (self.kind == "self_hosted" or bool(self.api_key))


@dataclass
class Policy:
    ai_policy: str
    residency_locked: bool = False

    def effective(self) -> str:
        return "self_hosted" if self.residency_locked else self.ai_policy

    def permitted_kinds(self) -> list[str]:
        return {"self_hosted": ["self_hosted"], "external": ["external"], "hybrid": ["self_hosted", "external"]}[self.effective()]


@dataclass
class Result:
    backend: str
    kind: str
    model: str
    content: str
    prompt_tokens: int
    completion_tokens: int
    attempts: list[dict] = field(default_factory=list)


def backends_from_env() -> list[Backend]:
    out = [Backend("vllm", "self_hosted", os.environ.get("SELF_HOSTED_LLM_URL", ""), os.environ.get("SELF_HOSTED_LLM_MODEL", ""),
                   os.environ.get("SELF_HOSTED_LLM_KEY", ""))]
    # External providers must be explicitly allow-listed: EXTERNAL_LLM_ALLOWLIST=name1,name2
    for name in filter(None, os.environ.get("EXTERNAL_LLM_ALLOWLIST", "").split(",")):
        p = name.upper()
        out.append(Backend(name, "external", os.environ.get(f"{p}_URL", ""), os.environ.get(f"{p}_MODEL", ""), os.environ.get(f"{p}_KEY", "")))
    return out


class Router:
    def __init__(self, backends: list[Backend], transport: httpx.BaseTransport | None = None, timeout: float = 60.0):
        self.backends = backends
        self.client = httpx.Client(transport=transport, timeout=timeout)

    def candidates(self, policy: Policy) -> list[Backend]:
        order = policy.permitted_kinds()
        return sorted((b for b in self.backends if b.kind in order and b.configured), key=lambda b: order.index(b.kind))

    def chat(self, policy: Policy, messages: list[dict]) -> Result:
        attempts: list[dict] = []
        for b in self.candidates(policy):
            t0 = time.monotonic()
            try:
                headers = {"Authorization": f"Bearer {b.api_key}"} if b.api_key else {}
                r = self.client.post(f"{b.base_url.rstrip('/')}/chat/completions", headers=headers,
                                     json={"model": b.model, "messages": messages, "temperature": 0.2})
                r.raise_for_status()
                body = r.json()
                usage = body.get("usage") or {}
                attempts.append({"backend": b.name, "ok": True, "ms": int((time.monotonic() - t0) * 1000)})
                return Result(b.name, b.kind, b.model, body["choices"][0]["message"]["content"],
                              int(usage.get("prompt_tokens", 0)), int(usage.get("completion_tokens", 0)), attempts)
            except (httpx.HTTPError, KeyError, ValueError) as e:
                attempts.append({"backend": b.name, "ok": False, "error": str(e)[:200]})
        raise Unavailable(f"no backend permitted by ai_policy={policy.effective()} is available; attempts={attempts}")
