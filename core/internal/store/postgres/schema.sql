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
    -- Money caps are in quota units (see pkg/billing). Money is derived at
    -- month end once the cost pool is known; storing a currency amount here
    -- would be storing a number Fleet cannot yet compute.
    budget_units bigint      NOT NULL DEFAULT 0,
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
    budget_units  bigint      NOT NULL DEFAULT 0,
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
    expires_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

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
    prompt_tokens      integer NOT NULL DEFAULT 0,
    completion_tokens  integer NOT NULL DEFAULT 0,
    cached_tokens      integer NOT NULL DEFAULT 0,
    reasoning_tokens   integer NOT NULL DEFAULT 0,

    -- Priced result, in millionths of a quota unit.
    amounts_micro bigint NOT NULL DEFAULT 0,

    -- False when the engine sent no usage and this row is an estimate. P6:
    -- the estimate is charged at max_tokens and reconciled afterwards, but the
    -- row records that it was an estimate so a reconciliation query can find
    -- it instead of it being invisible inside an aggregate.
    usage_known boolean NOT NULL DEFAULT true,

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
-- Reconciliation looks for estimates it has not yet fixed up.
CREATE INDEX IF NOT EXISTS usage_events_unestimated_idx
    ON usage_events (occurred_at)
    WHERE NOT usage_known;
