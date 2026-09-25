# Agent definition execution projection

Foundry is the source of truth for agent definitions (`internal/config/Definition`,
stored and versioned in Postgres). The worker's `internal/config/Definition`
is the JSON execution projection fetched by agent ID at activity start. Neither
Eve nor OpenDev owns a copy of the definition.

The projection includes identity, provider ID, kind, name, description, model,
system prompt, tools, turn and concurrency limits, JSON/structured output,
memory configuration, tool overrides, pre-inference processors, and handoffs.
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
