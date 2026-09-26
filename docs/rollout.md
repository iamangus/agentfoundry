# Coordinated rollout

1. Deploy Foundry with its Postgres migration. `agent_runs`,
   `agent_run_inputs`, and `chat_sessions` retain API ownership, input receipts,
   and visible sessions across restarts. Existing Temporal workflows continue
   using their workflow IDs; existing in-memory-only run IDs from an earlier
    Foundry process cannot be reconstructed retroactively.
   During a temporary Temporal outage, Foundry keeps watching existing runs;
   it treats a new workflow ID as a failed start only after it remains absent
   for a full minute.
2. Deploy the worker with `input_processed`/`input_failed` acknowledgements,
   turn-ID correlation, and persistent-workflow Continue-As-New. It accepts
   the same run parameters as the previous version; the new rollover fields
   are optional.
3. Deploy Eve. Its queue waits for the worker's input receipts; deploying it
   first would leave hidden events pending. Existing active turns complete
   before Eve cancels idle legacy `frontend:<conversation>` runs. Owner turns
   move to `frontend-v2:<conversation>` and `eve-chat`; hidden events retain
   their separate automation run and the full `eve` MCP server.
   An uncertain Temporal signal stays attached to its input ID; Eve retries
   that ID until Foundry confirms submission. Channel delivery failures retain
   the active turn and retry with persisted backoff.
 4. Deploy OpenDev's foundation gate. Owned non-fork repositories lacking a
   foundation receive a PR on provision or first coding job; ordinary catalog
   sync does not create PRs. Already-ready repositories retain their recorded
    foundation version until an explicit migration is requested.

For a later steering-only release on an installation with durable runs already
deployed, update the worker's Temporal steering handler first, Foundry's
conditional steering API second, and Eve last. Existing ordinary turns still
use signals during this rollout. A web follow-up is acknowledged only after
the worker accepts it into the current turn; late arrivals become queued turns.

CI runs the empty-database and restart/idempotency checks against Postgres,
the worker's in-memory Temporal workflow test, every repository's Go tests and
vet checks, and both frontend builds. Before promoting a release, exercise one
conversation restart and one pending foundation PR against the deployment's
real Temporal, PostgreSQL, and GitHub integrations; the local tests do not
replace those external systems.
