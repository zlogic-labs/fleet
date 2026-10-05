-- Fleet's authoritative schema.
--
-- Two rules govern this file.
--
-- 1. The ledger is append-only. There is no UPDATE or DELETE on ledger_entries
--    anywhere in the codebase, and no trigger or privilege that permits one.
--    A correction is a new row that references the one it corrects. If a
--    customer's invoice depends on this table, a silent mutation is worse than
--    a wrong number you can find later.
--
-- 2. Every row that is billed records its billing dimensions as facts, not as
--    references. usage_events carries tenant_id AND project_id AND key_id rather
--    than looking project up through the key, because a key's project
--    assignment is allowed to change — that is the point of rotating one team's
--    credential without moving its budget. Resolving project through key_id at
--    report time would attribute last month's usage to whichever project the
--    key points at now.

-- ── identity ───────────────────────────────────────────────────────────

-- A tenant is the billing relationship: one customer, one invoice.
--
-- The limit columns are an envelope, not a suggestion. Sub-limits held by
-- projects and keys partition this budget; they never add to it. Without that
-- rule, ten projects each declaring the tenant's full limit would give the
-- tenant ten times the capacity, and the envelope would be decorative.
CREATE TABLE IF NOT EXISTS tenants (
    id           text PRIMARY KEY,
    name         text        NOT NULL,
    -- The tenant's budget is not a column here but a set of rows in
    -- budget_rules. A single bigint could only ever express one dimension over
    -- one window, and the question "5M tokens per 5 hours, 200 units a month"
    -- has no answer without both.
    -- The envelope: the most this tenant may spend across every project it
    -- owns. Zero means unlimited.
    --
    -- These are not budget_units restated. A budget caps what may be *charged*;
    -- a rate limit caps what may be *in flight at once*. The gap between them
    -- is the whole reason both exist: a tenant can be well inside its monthly
    -- budget and still hold every GPU in the deployment for the whole month by
    -- sending requests that settle to nothing.
    request_limit integer     NOT NULL DEFAULT 0 CHECK (request_limit >= 0),
    token_limit   integer     NOT NULL DEFAULT 0 CHECK (token_limit   >= 0),
    active        boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- A project is a tenant's internal cost centre.
--
-- It has exactly one tenant (the column is not nullable and there is no join
-- table), and no nesting: a project cannot contain projects. Every extra level
-- multiplies the work in the limit arithmetic and in the rollups, and the
-- ledger cannot be un-nested later.
CREATE TABLE IF NOT EXISTS projects (
    id            text PRIMARY KEY,
    tenant_id     text        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name          text        NOT NULL,
    -- Nullable rather than "every tenant has a default project": a synthetic
    -- default row would show up in reports as a real cost centre and would have
    -- to be excluded from every one of them. NULL means "not attributed", and
    -- rollups COALESCE it to a single bucket.
    -- The partition: the most this project may spend, which must never exceed
    -- its tenant's envelope. That last rule is NOT a CHECK constraint, because
    -- it compares two rows in two tables. It is enforced on the write path
    -- (internal/gateway.limiterFor rejects it at startup for the config-backed
    -- case) and is the reason a project's limit is not simply additive: ten
    -- projects each declaring the tenant's full limit would give the tenant ten
    -- times the capacity and leave the envelope decorative.
    request_limit integer     NOT NULL DEFAULT 0 CHECK (request_limit >= 0),
    token_limit   integer     NOT NULL DEFAULT 0 CHECK (token_limit   >= 0),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS projects_tenant_name_idx ON projects (tenant_id, name);
CREATE INDEX IF NOT EXISTS projects_tenant_idx ON projects (tenant_id);

-- The limit columns, for a database created before they existed.
--
-- CREATE TABLE IF NOT EXISTS is a no-op on a table that is already there, so a
-- database that predates this change keeps its old shape forever and every
-- query naming request_limit fails. That is not only a 500: the gateway's
-- policy lookup is the same query, and it falls back to server defaults when
-- the lookup errors — so the deployment looks healthy and enforces the wrong
-- limits. Adding a column with a default does not rewrite existing rows, which
-- is the line db.go's Migrate draws between this file and a real migration.
ALTER TABLE tenants  ADD COLUMN IF NOT EXISTS request_limit integer NOT NULL DEFAULT 0;
ALTER TABLE tenants  ADD COLUMN IF NOT EXISTS token_limit   integer NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS request_limit integer NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS token_limit   integer NOT NULL DEFAULT 0;

-- budget_units was replaced by budget_rules, which can express a dimension and
-- a window rather than one number. Dropping it is safe and intentional: a
-- value there meant something different from anything the code now reads, and
-- leaving it invites someone to keep reading it.
ALTER TABLE tenants  DROP COLUMN IF EXISTS budget_units;
ALTER TABLE projects DROP COLUMN IF EXISTS budget_units;

-- An API key is a credential, not a scope.
--
-- It carries no limits at all. A key rotates, it leaks, and it is not a budget:
-- a limit attached to one would have to be reissued along with it, so lowering
-- a budget would invalidate whatever the caller was using when they were cut
-- off. Rotating a key keeps its project, so a rotation never moves a budget.
--
-- project_id is NOT NULL: a key that belongs to no project has no partition to
-- be limited in, and the resulting asymmetry — some keys charged to a project
-- and some only to the tenant — would make "how much can this project spend"
-- depend on which credential the caller happened to hold.
CREATE TABLE IF NOT EXISTS api_keys (
    id           text PRIMARY KEY,
    tenant_id    text        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id   text        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- The hash, never the key. A plaintext key column in a table that gets
    -- dumped into a support ticket is how a whole customer's access leaks.
    key_hash     bytea       NOT NULL UNIQUE,
    key_prefix   text        NOT NULL,  -- for display: "sk-fleet-a3f2…"
    label        text        NOT NULL DEFAULT '',
    -- Expiry and revocation are different states. An expired key could be
    -- extended; a revoked one leaked, and letting it come back by moving
    -- expires_at would turn a credential compromise into a scheduling
    -- decision.
    expires_at   timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Additive migrations, because CREATE TABLE IF NOT EXISTS is not one. It
-- creates a table that is absent and leaves an existing one exactly as it was,
-- so a column added after a deployment has shipped needs its own statement.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS revoked_at timestamptz;

CREATE INDEX IF NOT EXISTS api_keys_active_idx ON api_keys (project_id) WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS api_keys_tenant_idx  ON api_keys (tenant_id);
CREATE INDEX IF NOT EXISTS api_keys_project_idx ON api_keys (project_id);

-- ── pricing ────────────────────────────────────────────────────────────

-- One row per model per effective period.
--
-- History is kept rather than overwritten so that a month's usage can be
-- re-priced against the book that was in force when it happened. Deleting an
-- old price would make historical invoices unreproducible, which is the same
-- property the append-only ledger is protecting.
--
-- Rates are per million tokens, in quota units — matching the published
-- numbers operators recognise, and matching billing.UnitsPer. amounts_micro is
-- NOT stored: the money figure is derived from the month's cost pool and
-- cannot be known here.
CREATE TABLE IF NOT EXISTS price_books (
    id            text PRIMARY KEY,
    model         text        NOT NULL,
    input_rate    bigint      NOT NULL CHECK (input_rate > 0),
    output_rate   bigint      NOT NULL CHECK (output_rate > 0),
    -- A cache hit may not cost more than a miss. Enforced as a constraint so
    -- that a bad import cannot quietly invert a price.
    cached_rate   bigint      NOT NULL DEFAULT 0 CHECK (cached_rate >= 0 AND cached_rate <= input_rate),
    -- NULL means "same as output", matching billing.Rate.Reasoning. Storing 0
    -- would make the two disagree: Go reads 0 as "unset" and the SQL as "free".
    reasoning_rate bigint     CHECK (reasoning_rate IS NULL OR reasoning_rate >= 0),
    effective_from timestamptz NOT NULL,
    effective_to   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);

-- A model may have several books over time, so uniqueness is on the interval,
-- not on the model alone. Exclude-in-progress is a standard partial index trick:
-- without it, two open-ended books for one model could both exist and which one
-- applies would depend on read order.
CREATE UNIQUE INDEX IF NOT EXISTS price_books_one_open_per_model
    ON price_books (model)
    WHERE effective_to IS NULL;

CREATE INDEX IF NOT EXISTS price_books_lookup_idx ON price_books (model, effective_from DESC);

-- ── the ledger ─────────────────────────────────────────────────────────

-- One row per completed request.
--
-- The token columns are the raw numbers the engine reported, not the derived
-- fresh/cached split. Storing the raw figures means a change to the pricing
-- rule can be re-run over history; storing the split means a correction to the
-- rule cannot be applied to anything already written.
--
-- amounts_micro is the price in millionths of a quota unit. It is recomputable
-- from the token columns and the price book, and it is stored anyway so that a
-- report does not have to join a price book that may since have changed — the
-- row is a self-contained statement of what this request cost.
CREATE TABLE IF NOT EXISTS usage_events (
    id            bigserial PRIMARY KEY,
    -- Dimensions as facts, per the file header.
    tenant_id     text        NOT NULL,
    project_id    text,
    key_id        text,
    -- The resolved model — the one the endpoint served, not the string the
    -- client sent. Recorded so an alias that pointed somewhere expensive
    -- cannot be re-pointed to something cheap to rewrite last month's bill.
    model         text        NOT NULL,
    endpoint_id   text        NOT NULL DEFAULT '',
    -- The price book in force. A reference for audit; not used to re-price.
    price_book_id text,

    -- Raw engine-reported usage (P6: the only authority).
    --
    -- The two breakdown columns are nullable, and that is the point. An engine
    -- that reports totals without a breakdown has not said "zero cached
    -- tokens"; it has said nothing, and the difference decides whether the
    -- fresh/cached split on an invoice is a measurement or a guess. NOT NULL
    -- DEFAULT 0 would make the ledger assert the second for every engine that
    -- omits the field, which is most of them.
    --
    -- There is no DEFAULT either, so a writer that forgets the column stores
    -- the absence rather than a zero.
    prompt_tokens      integer NOT NULL DEFAULT 0,
    completion_tokens  integer NOT NULL DEFAULT 0,
    cached_tokens      integer,
    reasoning_tokens   integer,

    -- Priced result, in millionths of a quota unit.
    amounts_micro bigint NOT NULL DEFAULT 0,

    -- usage_known is false when the engine sent no usage at all. What the row
    -- was charged for instead is named by usage_source, so a report can ask
    -- how much of the token volume was measured rather than assumed without
    -- having to guess from the numbers.
    usage_known boolean NOT NULL DEFAULT true,
    usage_source text    NOT NULL DEFAULT 'engine',

    -- Performance, kept with the money because a report grouped by endpoint
    -- wants them and a join would otherwise drop them.
    ttft_ms      integer NOT NULL DEFAULT 0,
    duration_ms  integer NOT NULL DEFAULT 0,
    streamed     boolean NOT NULL DEFAULT false,

    occurred_at  timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CHECK (prompt_tokens >= 0 AND completion_tokens >= 0),
    CHECK (cached_tokens >= 0 AND cached_tokens <= prompt_tokens),
    CHECK (reasoning_tokens >= 0 AND reasoning_tokens <= completion_tokens),
    -- amounts_micro is a charge, never a credit. A negative here means the
    -- pricing rule went wrong, and the constraint makes it a failed insert
    -- rather than a quiet refund.
    CHECK (amounts_micro >= 0)
);

-- The rollups the product asks for, in the order the product asks for them.
--
-- One index cannot serve both "one tenant's spend this month" and "one model's
-- total across all tenants"; two indexes on a table this large is the honest
-- cost of not making the common report slow.
CREATE INDEX IF NOT EXISTS usage_events_tenant_time_idx
    ON usage_events (tenant_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS usage_events_project_time_idx
    ON usage_events (project_id, occurred_at DESC)
    WHERE project_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS usage_events_model_time_idx
    ON usage_events (model, occurred_at DESC);

-- Dropping a NOT NULL constraint rewrites no rows, so it belongs here beside
-- the CREATE rather than in a migration. Rows written before this had the
-- zeros an earlier version asserted on the engine's behalf; they keep them,
-- because a row that says zero cannot be turned back into a row that says
-- nothing, and inventing the absence would be the larger lie.
ALTER TABLE usage_events ALTER COLUMN cached_tokens    DROP NOT NULL;
ALTER TABLE usage_events ALTER COLUMN cached_tokens    DROP DEFAULT;
ALTER TABLE usage_events ALTER COLUMN reasoning_tokens DROP NOT NULL;
ALTER TABLE usage_events ALTER COLUMN reasoning_tokens DROP DEFAULT;

-- usage_source names what a row was charged on: the engine's own report, the
-- gateway's count of the answer it forwarded, or the reservation.
--
-- An existing row is any one of those and this file cannot know which, so it
-- gets the default that is right for the overwhelming majority: usage_known was
-- true for every row written before a column distinguishing them existed, and
-- those rows really were the engine's own figures.
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS usage_source text NOT NULL DEFAULT 'engine';

-- truncated marks a counted answer that hit the tap's 256 KiB cap. The token
-- count on such a row is a floor, so it sits below what the engine would have
-- said for the same request; recording that is what keeps the agreement check
-- from reporting arithmetic as a metering fault.
--
-- False for every existing row, and correctly so: an old row was either the
-- engine's own figure or a whole-body parse under a cap that predates this.
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS truncated boolean NOT NULL DEFAULT false;

-- Reconciliation looks for rows that were charged on something other than the
-- engine's account and have not been contradicted since. usage_known covers
-- counted and reserved alike, so a reconcile pass finds both; usage_source is
-- what it reads to tell them apart.
CREATE INDEX IF NOT EXISTS usage_events_unestimated_idx
    ON usage_events (occurred_at)
    WHERE NOT usage_known;

-- ── budgets ────────────────────────────────────────────────────────────────
--
-- A budget is a set of rules, not a number. Each rule is "this much of this
-- thing per this much time", and a scope is refused as soon as one of its rules
-- would be passed. That is what makes a tenant enforceable on more than one
-- axis at a time: 5M tokens per 5 hours AND 200 quota units per month are two
-- rules on the same scope, and the cheaper-to-express one is not the
-- substitute for the other.
--
-- Dimensions exist because "tokens" is not one number. Prompt and completion
-- are priced differently, and the cached part of a prompt is priced differently
-- again, so a rule on tokens has to say which of them it means. fresh is the
-- prompt that was not served from the engine's prefix cache — the only part of
-- a prompt that costs full input price, and on a deployment where caching works
-- well it is a small fraction, so a budget stated in total tokens would
-- overstate the cost by an order of magnitude.
--
-- A scope with no rows is unlimited. That is the whole default: an evaluation,
-- a laptop, and every tenant nobody has thought about yet all serve traffic
-- with no budget attached, and no row is the natural way to say so.

CREATE TABLE IF NOT EXISTS budget_rules (
    id       text PRIMARY KEY,
    -- tenant or project, matching quota.ScopeKind. NOT NULL-constrained
    -- because an untyped rule cannot be applied to anything and would be
    -- invisible until somebody wondered why it had no effect.
    scope_kind text        NOT NULL CHECK (scope_kind IN ('tenant', 'project')),
    scope_id   text        NOT NULL,
    -- Which quantity this rule caps. See quota.Dimension.
    dimension  text        NOT NULL,
    -- The allowance, in the dimension's own unit: tokens for the token
    -- dimensions, millionths of a quota unit for 'units'.
    limit_value bigint      NOT NULL CHECK (limit_value >= 0),
    -- The window, in seconds, and the bucket size in seconds.
    --
    -- Both are stored rather than only the window because the resolution is
    -- derived from the duration, and a derived value that is recomputed on
    -- every read is a value that can drift. Storing it means a rule keeps
    -- meaning the same thing even if the derivation is retuned.
    window_seconds   integer NOT NULL CHECK (window_seconds > 0),
    resolution_seconds integer NOT NULL CHECK (resolution_seconds > 0),
    created_at timestamptz NOT NULL DEFAULT now(),

    CHECK (resolution_seconds <= window_seconds)
);

-- The check looks up every rule that applies to a scope.
CREATE INDEX IF NOT EXISTS budget_rules_scope_idx
    ON budget_rules (scope_kind, scope_id);

-- One tenant cannot have two rules for the same dimension and window: the two
-- would be enforced independently and the tighter would silently win while
-- both read as configured.
CREATE UNIQUE INDEX IF NOT EXISTS budget_rules_unique_idx
    ON budget_rules (scope_kind, scope_id, dimension, window_seconds);

-- Running spend per scope, in time buckets.
--
-- A bucket, not a single running total, because the windows are rolling and a
-- rolling window has no fixed start. "The last 5 hours" cannot be one counter
-- that resets somewhere; it is a sum over the buckets that fall inside it. The
-- bucket size is chosen per rule so the number of buckets a check has to add up
-- stays bounded whatever the duration — see quota.ResolutionFor.
--
-- All six dimensions live in one row rather than a row per dimension: a check
-- that spans several dimensions reads them together, and six narrow rows per
-- bucket per scope is six times the rows for no read saved.
CREATE TABLE IF NOT EXISTS spend_counters (
    scope        text        NOT NULL,
    -- The bucket this row covers, aligned to resolution_seconds. Buckets
    -- older than the longest window any rule needs are deleted, not kept.
    bucket_start timestamptz NOT NULL,

    tokens_total_spent     bigint NOT NULL DEFAULT 0,
    tokens_total_reserved  bigint NOT NULL DEFAULT 0,
    tokens_input_spent     bigint NOT NULL DEFAULT 0,
    tokens_input_reserved  bigint NOT NULL DEFAULT 0,
    tokens_output_spent    bigint NOT NULL DEFAULT 0,
    tokens_output_reserved bigint NOT NULL DEFAULT 0,
    tokens_cached_spent    bigint NOT NULL DEFAULT 0,
    tokens_cached_reserved bigint NOT NULL DEFAULT 0,
    tokens_fresh_spent     bigint NOT NULL DEFAULT 0,
    tokens_fresh_reserved  bigint NOT NULL DEFAULT 0,
    units_spent_micro    bigint NOT NULL DEFAULT 0,
    units_reserved_micro bigint NOT NULL DEFAULT 0,

    PRIMARY KEY (scope, bucket_start)
);

-- Every check is a range sum on this, and it is the only index the read path
-- needs. It is also the primary key, so it is free.
--
-- None of these may go negative. A counter that did would silently raise the
-- scope's available budget, which is the one direction of error that pays out
-- money.
ALTER TABLE spend_counters DROP CONSTRAINT IF EXISTS spend_counters_nonnegative;
ALTER TABLE spend_counters ADD CONSTRAINT spend_counters_nonnegative CHECK (
    tokens_total_spent     >= 0 AND tokens_total_reserved  >= 0 AND
    tokens_input_spent     >= 0 AND tokens_input_reserved  >= 0 AND
    tokens_output_spent    >= 0 AND tokens_output_reserved >= 0 AND
    tokens_cached_spent    >= 0 AND tokens_cached_reserved >= 0 AND
    tokens_fresh_spent     >= 0 AND tokens_fresh_reserved  >= 0 AND
    units_spent_micro    >= 0 AND units_reserved_micro >= 0
);

-- The sliding minute, for the rate limiter.
--
-- Deliberately not spend_counters: those buckets are sized to whichever window
-- a budget rule declares, and a rule summing over its window would pick up a
-- rate limiter's one-second rows and count them against the budget. The rate
-- limit is a fixed sixty seconds by definition, so one-second buckets are
-- exact rather than inferred.
--
-- reserved and settled are separate for the same reason they are there: a check
-- that read only reserved would see the bucket drain after every request
-- finishes, and a steady stream of short requests would never reach a ceiling.
CREATE TABLE IF NOT EXISTS rate_counters (
    scope    text   NOT NULL,
    second   bigint NOT NULL,
    requests bigint NOT NULL DEFAULT 0,
    reserved bigint NOT NULL DEFAULT 0,
    settled  bigint NOT NULL DEFAULT 0,
    -- inflight counts reservations outstanding, charged to the second the
    -- request arrived in and summed without the window filter: a request
    -- either is running or is not, and ageing it out would report work that is
    -- still generating as though it had finished.
    inflight bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, second)
);

-- Nothing here may go negative. A negative counter raises a scope's apparent
-- headroom, which is the one direction of error that lets a tenant through.
ALTER TABLE rate_counters DROP CONSTRAINT IF EXISTS rate_counters_nonnegative;
ALTER TABLE rate_counters ADD CONSTRAINT rate_counters_nonnegative CHECK (
    requests >= 0 AND reserved >= 0 AND settled >= 0
);
-- What one GPU-hour costs the operator (P8).
--
-- Fleet cannot know this: a cloud bill, a colocation contract and a
-- depreciated on-prem fleet have nothing in common. It is declared, per
-- cluster, because a heterogeneous fleet does not have one rate.
CREATE TABLE IF NOT EXISTS cost_rates (
    cluster        text PRIMARY KEY,
    gpu_hour_micro bigint      NOT NULL CHECK (gpu_hour_micro >= 0),
    currency       text        NOT NULL DEFAULT 'USD',
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Capacity as it was, over time.
--
-- This is what turns "we have eight GPUs" into "we had eight GPUs for three
-- hours", and it only exists because the inventory report is append-only here.
-- Without it the cost pool can be computed for the instant Fleet started
-- watching and for no period at all.
CREATE TABLE IF NOT EXISTS capacity_samples (
    cluster   text        NOT NULL,
    at        timestamptz NOT NULL,
    gpu_count bigint      NOT NULL CHECK (gpu_count >= 0),
    PRIMARY KEY (cluster, at)
);

CREATE TABLE IF NOT EXISTS deployment_samples (
    deployment      text        NOT NULL,
    cluster         text        NOT NULL,
    at              timestamptz NOT NULL,
    -- gpu_per_replica is kept beside the total so a request can be priced
    -- against the shape of the deployment as it was when the request ran.
    -- Joining the current shape instead would silently re-price a month every
    -- time somebody scaled a replica.
    gpu_per_replica integer     NOT NULL DEFAULT 1 CHECK (gpu_per_replica >= 0),
    gpu_count       bigint      NOT NULL CHECK (gpu_count >= 0),
    PRIMARY KEY (deployment, at)
);

CREATE INDEX IF NOT EXISTS deployment_samples_at_idx ON deployment_samples (at);
CREATE INDEX IF NOT EXISTS usage_events_occurred_idx ON usage_events (occurred_at);

-- A closed period, immutable.
--
-- A period is frozen once a later period has been closed, because that is the
-- point at which a correction would have nowhere to land: the difference has to
-- be collected by a month that comes after, and after October is invoiced there
-- is no such month for September. Until then it can be recomputed, and the
-- movement is booked to the month immediately after it — the same reason the
-- ledger is append-only. An invoice that can change after it was sent is not an
-- invoice.
CREATE TABLE IF NOT EXISTS cost_periods (
    period           text PRIMARY KEY,
    closed_at        timestamptz NOT NULL DEFAULT now(),
    currency         text        NOT NULL DEFAULT '',
    priced           boolean     NOT NULL DEFAULT false,
    pool_micro       bigint      NOT NULL DEFAULT 0,
    busy_micro       bigint      NOT NULL DEFAULT 0,
    idle_micro       bigint      NOT NULL DEFAULT 0,
    idle_percent     integer     NOT NULL DEFAULT 0,
    coverage_percent integer     NOT NULL DEFAULT 0,
    pool_gpu_seconds bigint      NOT NULL DEFAULT 0,
    revision         integer     NOT NULL DEFAULT 1
);

ALTER TABLE cost_periods    ADD COLUMN IF NOT EXISTS revision integer NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS cost_allocations (
    period           text NOT NULL,
    scope            text NOT NULL,
    gpu_seconds      bigint NOT NULL DEFAULT 0,
    share            bigint NOT NULL DEFAULT 0,
    amount_micro     bigint NOT NULL DEFAULT 0,
    usage_micro      bigint NOT NULL DEFAULT 0,
    revision         integer NOT NULL DEFAULT 1,
    adjustment_micro bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (period, scope)
);

-- The allocation table holds the current revision only. Superseded values are
-- not kept: every difference between two revisions is written to
-- cost_adjustments as it happens, and current minus the recorded movement is the
-- previous figure. Keeping a second copy of every row would grow without bound
-- for a number that is already exactly determined.
ALTER TABLE cost_allocations ADD COLUMN IF NOT EXISTS revision integer NOT NULL DEFAULT 1;
ALTER TABLE cost_allocations ADD COLUMN IF NOT EXISTS adjustment_micro bigint NOT NULL DEFAULT 0;

-- A correction carried from an already-closed period into the one that found it.
--
-- This is the only thing in Fleet that looks like a debt. There is no balance to
-- roll forward: a period apportions a pool that has already been paid for, and
-- a request is reserved and accounted for together, so nothing can be spent in
-- one month and paid in another. What can happen is that a month is closed
-- before every observation for it arrived, and the only honest repair is to leave
-- the invoice alone and collect the difference now.
CREATE TABLE IF NOT EXISTS cost_adjustments (
    period       text NOT NULL,
    for_period   text NOT NULL,
    scope        text NOT NULL,
    gpu_seconds  bigint NOT NULL DEFAULT 0,
    amount_micro bigint NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (period, for_period, scope)
);

CREATE INDEX IF NOT EXISTS cost_adjustments_corrected_idx ON cost_adjustments (for_period);

-- Where the backfill into the detail store has reached.
--
-- A single row keyed by name rather than a column on usage_events, because it is
-- state about the replica and not about a request. Storing it here rather than in
-- the replica keeps the authoritative table untouched by the fact that a replica
-- exists, which is the whole reason the replica can be dropped and rebuilt.
--
-- The value is text because it is read with COALESCE against a default and
-- written as text; a bigint would need a cast on the read path and would fail
-- there rather than here if the row were ever hand-edited.
CREATE TABLE IF NOT EXISTS detail_state
(
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
