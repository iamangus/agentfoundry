# Agent definition execution projection

Foundry is the source of truth for agent definitions (`internal/config/Definition`,
stored and versioned in Postgres). The worker's `internal/config/Definition`
is the JSON execution projection fetched by agent ID at activity start. Neither
Eve nor OpenDev owns a copy of the definition.

The projection includes identity, provider ID, kind, name, description, model,
system prompt, tools, turn and concurrency limits, JSON/structured output,
memory configuration, tool overrides, pre-inference processors, and handoffs.
Optional `backup_provider_id` and `backup_model` select an alternative inference
route. A blank backup model uses that provider's default. The backup must be
visible to the editor and compatible with the agent's sharing scope.
Foundry-owned `scope`, `team`, and `created_by` authorize and catalogue the
definition and do not belong in execution state. Foundry-owned `model_params`
are merged into the inference request by Foundry's inference endpoint, not by
the worker. The worker must not silently duplicate that merge.

Wire-format rules:

- Definition JSON field names, processor configuration, and structured-output
  schema are the contract; YAML is an editing format, not the worker protocol.
- Adding a field used during execution requires updating both definitions and
  testing a JSON round trip through the worker projection. Additive fields that
  the worker ignores can be deployed on Foundry first.
- Changing the meaning of an existing field requires a versioned protocol or
  coordinated rollout; an unknown execution field must not be used as a
  substitute for an unrelated Foundry-owned field.
- Run, turn, conversation, task, and coding-job IDs remain separate domain
  identifiers; correlation follows `docs/integration-contract.md`.

## Backup execution contract

- With a backup configured, primary inference has exactly two attempts for
  transient failures, then backup inference has at most two attempts. Permanent
  request/authentication failures and cancellation do not trigger failover.
- Backup selection is sticky for that run, including persistent turns and
  Continue-As-New. Child agents independently resolve their own definitions.
- The worker sends `X-AgentFoundry-Backup: true` to the internal inference
  endpoint. Foundry chooses only the configured backup, applies its default
  model/reasoning, and does not merge primary `model_params` into backup calls.
- Foundry adapts native structured-output requests to JSON mode plus schema
  instructions when a selected provider does not support native validation.
  Worker output validation remains responsible for enforcing the schema.
- Inference response headers identify the actual provider and model. The
  normalized trace exposes `provider_id`, `model`, `attempts`, `backup`, and
  `failover_reason`. No credentials belong in these fields.
- Deploy API routing before enabling backups on updated workers. Temporal's
  `backup-inference-v1` marker retains replay behavior for older histories.
