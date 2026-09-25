# Execution and delivery contract

The services have different owners of truth. Agent Foundry owns definitions,
provider and MCP configuration, API run identity, authorization, and the
run-to-Temporal workflow mapping. Temporal owns execution history. Eve owns
visible conversations, the origin and destination of each message, and its
internal event queue. OpenDev owns coding jobs, dispatch intents, repository
foundation state, validation results, and its notification outbox.

## Identifiers

| Identifier | Issuer | Purpose |
| --- | --- | --- |
| `run_id` | Foundry | Authorized API handle for one workflow; persistent runs have multiple inputs. |
| `workflow_id` | Foundry/Temporal | Execution identity, not authorization. Continue-As-New retains its workflow ID. |
| `input_id` | Client | Idempotency key for an input to a persistent run; stable across retries. |
| `source_id` | Eve channel adapter | Optional upstream message/event identity used to deduplicate inbound delivery before assigning a local message ID. |
| `turn_id` | Eve | Correlates a delivered assistant reply to the originating message, even when many turns share one persistent run. |
| `client_key` | Client | Optional owner-scoped idempotency key for creating an active persistent run. Eve uses its conversation scope. |
| Eve message ID | Eve | Visible turn and reply destination; stored with the active run association. |
| `task_id` | Eve or OpenDev | Recover a submitted one-shot run through Foundry's indexed run lookup. |
| OpenDev job/dispatch ID | OpenDev | Deterministic state machine and individual agent attempt. |
| OpenDev event ID | OpenDev | Outbox and Eve queue deduplication key; the first Foundry `input_id` uses this ID, and confirmed agent failures get distinct retry input IDs. |

Eve uses separate `frontend-v2:<conversation>` and
`frontend-events:<conversation>` client keys. The visible web run has chat-only
MCP tools; hidden frontend events have automation tools and do not silently
broaden the web run's privileges.
On upgrade Eve cancels the legacy `frontend:<conversation>` run once it is idle,
then clears that reference. In-flight legacy turns finish before cancellation.
Owner inputs from web, email, Matrix, SMS, and voice enter one persisted
per-conversation turn queue. The first turn on a new run bootstraps earlier
history once; subsequent turns signal the persistent workflow with stable
`chat-<conversation>-<message>` input IDs. The user's reply destination belongs
to that message, not the latest channel seen by the conversation.

Foundry stores run ownership, status, result, UI chat sessions, and input receipts in Postgres.
The owner-scoped `GET /api/v1/runs/{id}/inputs/{inputID}` reports `accepted`,
`processed`, or `failed`. A successful input submission means Temporal accepted
the signal; an event is *processed* only after the worker has finished the
turn that included that input. Eve completes a queued internal event only after
receiving the processed receipt. On a timeout it retries with the same ID.
OpenDev does not mark an outbox notification delivered when Eve merely accepts
the webhook: it checks Eve's authenticated durable event status, polling until
`done`. Eve's terminal `failed` status remains inspectable in both services.
Foundry records deterministic Temporal workflow IDs before submitting work, so
an uncertain workflow-start response cannot erase its discoverable run record.
For new one-shot submissions, a repeated owner-scoped `task_id` returns the
same run, including its terminal result; a new attempt needs a new `task_id`.

One-shot runs become `running`, then `completed`, `failed`, or `canceled`.
Persistent runs alternate between `waiting` and `running`; their `run_id`
stays stable while each input has its own receipt and terminal outcome.
Foundry persists results before publishing the corresponding SSE terminal
event. SSE token replay is bounded and is not the authoritative result store.

## Repository readiness

OpenDev catalog lookup and startup sync are non-mutating. Provisioning an
owned, non-fork repository or creating its first coding job enforces the
versioned foundation. The job is held until the foundation PR's current head
passes its required CI check and merges. OpenDev reconciles pending foundation
PRs even when no job has yet been created. Forks and foreign repositories are
exempt. A failed or closed PR is blocked; a ready older foundation is not
silently migrated to a newer version. Only the admin-only
`migrate_repository_foundation` operation initiates a later version's PR.

Eve's Matrix reply retries reuse a transaction ID. Email retries reuse a
Message-ID, but SMTP delivery is at-least-once across an ambiguous acceptance.
The active turn and its reply destination stay persisted during external
delivery failures; later conversation inputs remain queued until delivery.

## Change procedure

Keep the JSON names of existing fields stable when evolving Foundry/worker
workflow inputs, Foundry/consumer APIs, or OpenDev/Eve events. Add new optional
fields before making producers depend on them; deploy Foundry database
migrations before updating the worker and consumers. Exercise fresh and
repeated Postgres migrations, a persistent run restart, a delayed turn result,
an uncertain duplicate input submission, and a foundation PR passing and
failing CI. Keep historical visible Eve messages separate from hidden
internal-event inputs.
