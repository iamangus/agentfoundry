# Inference metrics

The inference proxy persists one metadata-only record per upstream HTTP attempt in PostgreSQL. No prompts, responses, API keys, or provider headers are included. Temporal execution/activity/attempt correlation gives retries separate identities and makes repeated saves idempotent. Records survive workflow Continue-As-New.

`GET /api/v1/runs/{id}/inference?input_id=...&cursor=...` requires the run owner or a global administrator. `input_id` selects a persistent turn. Pages contain up to 200 attempts, newest first, and an opaque `next_cursor`. Clients must follow every page before treating totals as complete. There is no automatic retention policy yet; records remain until explicitly managed by the database operator.

Token counts, cached prompt tokens and reasoning tokens are reported by the upstream provider; omitted counts are unknown, not zero. Cache hit ratio is cached/input tokens. Reply totals include all attempts and retries; the aggregate cache ratio uses token-weighted totals, not a mean of percentages.

Proxy timing starts immediately before upstream dispatch. TTFT includes reasoning and tool-call deltas; first visible text is separate. Role-only deltas do not count. Observed generation throughput is `(output_tokens - 1) / (last_generated_chunk_time - first_generated_chunk_time)`. Batching and network buffering affect this value; it is not exact GPU decode speed. Nonstreamed requests do not have observed TTFT or generation throughput. Total duration includes upstream response forwarding, but excludes the metrics database write.

llama.cpp `timings.prompt_ms`, `prompt_n` and `predicted_ms` populate provider prefill/decode measurements when available. Queue/time-to-start-prefill stays absent unless authoritative instrumentation exists. The deployed SGLang OpenAI streaming API does not provide exact request prefill or queue timings; no subtraction or estimate is used to invent them.

`GET /api/v1/inference/providers` lists caller-visible providers without credentials. Add `provider_id` to sample an individual provider. Custom/SGLang endpoints are probed at `/metrics`, replacing trailing `/v1`; hosted-provider dashboards are not scraped. Scrapes refuse redirects, have a four-second deadline and a 2 MiB response limit. Only allowlisted finite numeric SGLang metrics are returned.

Provider queue depth is instantaneous. Queue/TTFT means and cached/prompt ratio are server-lifetime aggregates. Compute-prefill and decode throughput use counter deltas across samples 1–120 seconds apart, and represent aggregate server throughput, not per-request token-processing speed. The first sample, a counter reset, or a stale sample leaves rates unavailable. Eve samples every ten seconds while its Inference page is open. A `custom` provider without readable SGLang metrics reports unavailable.

Verification: `GOTOOLCHAIN=auto go test -race ./...`. To exercise database migrations, idempotency and cursor pagination, set `INFERENCE_TEST_DB_URL` to a disposable PostgreSQL database; the test creates and drops its own isolated schema.
