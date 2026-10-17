import { test } from "node:test";
import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { Paystack, Flutterwave, creditCore } from "../src/providers.js";

const fakeFetch = (data: unknown, ok = true) => (async () => ({ ok, status: ok ? 200 : 400, json: async () => data, text: async () => "" })) as unknown as typeof fetch;

test("unconfigured providers report unavailable and reject webhooks", () => {
  const p = new Paystack("");
  assert.equal(p.configured(), false);
  assert.equal(p.verifyWebhook("{}", { "x-paystack-signature": "x" }), false);
  assert.equal(new Flutterwave("", "").configured(), false);
});

test("paystack webhook signature is HMAC-SHA512 of the raw body", () => {
  const p = new Paystack("sk_test_x");
  const body = '{"event":"charge.success"}';
  const sig = createHmac("sha512", "sk_test_x").update(body).digest("hex");
  assert.equal(p.verifyWebhook(body, { "x-paystack-signature": sig }), true);
  assert.equal(p.verifyWebhook(body + " ", { "x-paystack-signature": sig }), false);
});

test("paystack verify only accepts successful NGN charges with an org", async () => {
  const ok = new Paystack("sk", fakeFetch({ status: true, data: { status: "success", currency: "NGN", amount: 500000, reference: "R1", metadata: { org_id: "o1" } } }));
  assert.deepEqual((await ok.verify("R1"))?.amountMinor, 500000);
  const failed = new Paystack("sk", fakeFetch({ status: true, data: { status: "failed", currency: "NGN", amount: 1, reference: "R2", metadata: { org_id: "o1" } } }));
  assert.equal(await failed.verify("R2"), null);
});

test("flutterwave converts naira to kobo exactly", async () => {
  const f = new Flutterwave("sk", "hash", fakeFetch({ status: "success", data: { status: "successful", currency: "NGN", amount: 1234.56, tx_ref: "T", meta: { org_id: "o" } } }));
  assert.equal((await f.verify("T"))?.amountMinor, 123456);
});

test("creditCore refuses to run without the internal secret", async () => {
  await assert.rejects(creditCore({ provider: "paystack", providerRef: "r", orgId: "o", amountMinor: 1, currency: "NGN", raw: {} }, "http://x", ""));
});

test("CORS is granted only to configured client origins", async () => {
  const { corsHeaders } = await import("../src/cors.js");
  assert.equal(corsHeaders("http://localhost:5173")["Access-Control-Allow-Origin"], "http://localhost:5173");
  assert.deepEqual(corsHeaders("https://evil.example"), {});
  assert.deepEqual(corsHeaders(undefined), {});
});
