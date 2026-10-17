import httpx
import pytest

from ai_devops.router import Backend, Policy, Router, Unavailable

SH = Backend("vllm", "self_hosted", "http://sh", "qwen")
EX = Backend("openai", "external", "http://ex", "gpt", "k")


def transport(down: set[str]):
    def handler(req: httpx.Request):
        if req.url.host in down:
            return httpx.Response(503)
        return httpx.Response(200, json={"choices": [{"message": {"content": req.url.host}}], "usage": {"prompt_tokens": 3, "completion_tokens": 2}})
    return httpx.MockTransport(handler)


def test_self_hosted_never_falls_back_to_external():
    r = Router([SH, EX], transport({"sh"}))
    with pytest.raises(Unavailable):
        r.chat(Policy("self_hosted"), [])


def test_hybrid_prefers_self_hosted_then_falls_back():
    assert Router([EX, SH], transport(set())).chat(Policy("hybrid"), []).backend == "vllm"
    res = Router([SH, EX], transport({"sh"})).chat(Policy("hybrid"), [])
    assert res.backend == "openai" and res.attempts[0]["ok"] is False


def test_residency_lock_overrides_policy():
    with pytest.raises(Unavailable):
        Router([EX], transport(set())).chat(Policy("external", residency_locked=True), [])


def test_external_requires_key():
    assert Router([Backend("x", "external", "http://ex", "m")], transport(set())).candidates(Policy("external")) == []
