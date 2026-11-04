-- Sway schema v1. All money is integer cents (bigint). Ledger and attributions are append-only (enforced by triggers).

CREATE TABLE merchants (
  id                     text PRIMARY KEY,
  name                   text NOT NULL,
  paypal_merchant_email  text NOT NULL DEFAULT '',
  commission_rate_bps    int  NOT NULL CHECK (commission_rate_bps BETWEEN 0 AND 10000),
  vesting_window_seconds int  NOT NULL CHECK (vesting_window_seconds >= 0),
  created_at             timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE products (
  id           text PRIMARY KEY,
  merchant_id  text NOT NULL REFERENCES merchants(id),
  name         text NOT NULL,
  brand        text NOT NULL,
  category     text NOT NULL,
  price_cents  bigint NOT NULL CHECK (price_cents > 0),
  specs        jsonb NOT NULL DEFAULT '{}'::jsonb,
  tagline      text NOT NULL DEFAULT '',
  image_url    text NOT NULL DEFAULT '',
  source       text NOT NULL DEFAULT 'curated' CHECK (source IN ('curated','channel3')),
  external_id  text,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX products_category_idx ON products(category);

CREATE TABLE creators (
  id            text PRIMARY KEY,
  display_name  text NOT NULL,
  paypal_email  text NOT NULL,
  verified      boolean NOT NULL DEFAULT false,
  bio           text NOT NULL DEFAULT '',
  avatar_hue    int NOT NULL DEFAULT 200,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sources (
  id               text PRIMARY KEY,
  creator_id       text NOT NULL REFERENCES creators(id),
  type             text NOT NULL CHECK (type IN ('review','blog','forum','transcript')),
  title            text NOT NULL,
  body             text NOT NULL,
  published_at     timestamptz NOT NULL,
  status           text NOT NULL DEFAULT 'active' CHECK (status IN ('active','blocked_injection','duplicate')),
  status_reason    text NOT NULL DEFAULT '',
  duplicate_of_id  text REFERENCES sources(id),
  similarity       double precision,
  screened_by      text NOT NULL DEFAULT '',
  created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE source_products (
  source_id   text NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  product_id  text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  PRIMARY KEY (source_id, product_id)
);
CREATE INDEX source_products_product_idx ON source_products(product_id);

CREATE TABLE shopping_sessions (
  id            text PRIMARY KEY,
  request_text  text NOT NULL,
  budget_cents  bigint,
  category      text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE decisions (
  id                 text PRIMARY KEY,
  session_id         text NOT NULL REFERENCES shopping_sessions(id),
  request_text       text NOT NULL,
  candidate_ids      jsonb NOT NULL,
  source_ids         jsonb NOT NULL,
  excluded_sources   jsonb NOT NULL DEFAULT '[]'::jsonb,
  chosen_product_id  text NOT NULL REFERENCES products(id),
  probabilities      jsonb NOT NULL DEFAULT '{}'::jsonb,
  rationale          text NOT NULL,
  relied_on_ids      jsonb NOT NULL DEFAULT '[]'::jsonb,
  model              text NOT NULL,
  prompt_version     text NOT NULL,
  engine             text NOT NULL,
  status             text NOT NULL DEFAULT 'PROPOSED' CHECK (status IN ('PROPOSED','PURCHASED','ABANDONED')),
  influence          jsonb,
  created_at         timestamptz NOT NULL DEFAULT now()
);

-- Cache of every agent run. Counterfactuals are only comparable when model + prompt + inputs match exactly.
CREATE TABLE llm_runs (
  cache_key             text PRIMARY KEY,
  model                 text NOT NULL,
  prompt_version        text NOT NULL,
  coalition_source_ids  jsonb NOT NULL,
  sample_index          int NOT NULL,
  scores                jsonb NOT NULL,
  chosen_product_id     text NOT NULL,
  rationale             text NOT NULL,
  relied_on_ids         jsonb NOT NULL DEFAULT '[]'::jsonb,
  latency_ms            int NOT NULL DEFAULT 0,
  created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orders (
  id                   text PRIMARY KEY,
  decision_id          text REFERENCES decisions(id),
  product_id           text NOT NULL REFERENCES products(id),
  merchant_id          text NOT NULL REFERENCES merchants(id),
  amount_cents         bigint NOT NULL CHECK (amount_cents > 0),
  commission_rate_bps  int NOT NULL,
  sway_fee_bps         int NOT NULL,
  paypal_order_id      text UNIQUE,
  paypal_capture_id    text UNIQUE,
  payment_mode         text NOT NULL CHECK (payment_mode IN ('sandbox','test','historical')),
  status               text NOT NULL DEFAULT 'CREATED' CHECK (status IN ('CREATED','APPROVED','CAPTURED','REFUNDED','PARTIALLY_REFUNDED','FAILED')),
  dispute_status       text NOT NULL DEFAULT 'NONE' CHECK (dispute_status IN ('NONE','OPEN','RESOLVED_SELLER','RESOLVED_BUYER')),
  dispute_test    boolean NOT NULL DEFAULT false,
  refunded_cents       bigint NOT NULL DEFAULT 0 CHECK (refunded_cents >= 0),
  captured_at          timestamptz,
  vests_at             timestamptz,
  vested_at            timestamptz,
  buyer_email          text NOT NULL DEFAULT '',
  historical               boolean NOT NULL DEFAULT false,
  created_at           timestamptz NOT NULL DEFAULT now(),
  CHECK (refunded_cents <= amount_cents)
);
CREATE INDEX orders_vesting_idx ON orders(status, vests_at) WHERE vested_at IS NULL;

CREATE TABLE attribution_jobs (
  id               text PRIMARY KEY,
  order_id         text NOT NULL UNIQUE REFERENCES orders(id),
  status           text NOT NULL DEFAULT 'QUEUED' CHECK (status IN ('QUEUED','RUNNING','DONE','FAILED')),
  phase            text NOT NULL DEFAULT 'queued',
  progress         int NOT NULL DEFAULT 0,
  total_runs       int NOT NULL DEFAULT 0,
  cache_hits       int NOT NULL DEFAULT 0,
  method           text NOT NULL DEFAULT 'exact',
  approximate      boolean NOT NULL DEFAULT false,
  v_full           double precision,
  v_none           double precision,
  share            double precision,
  efficiency_gap   double precision,
  players          jsonb,
  coalitions       jsonb,
  error            text NOT NULL DEFAULT '',
  started_at       timestamptz,
  finished_at      timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE attributions (
  id            text PRIMARY KEY,
  order_id      text NOT NULL REFERENCES orders(id),
  source_id     text NOT NULL REFERENCES sources(id),
  creator_id    text NOT NULL REFERENCES creators(id),
  phi           double precision NOT NULL,
  phi_stddev    double precision NOT NULL DEFAULT 0,
  loo_delta     double precision NOT NULL DEFAULT 0,
  weight        double precision NOT NULL DEFAULT 0,
  amount_cents  bigint NOT NULL DEFAULT 0 CHECK (amount_cents >= 0),
  role          text NOT NULL DEFAULT 'player' CHECK (role IN ('player','background')),
  explanation   text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (order_id, source_id)
);
CREATE INDEX attributions_creator_idx ON attributions(creator_id);

-- Double-entry, hash-chained ledger. Every entry moves amount (>0) from one account to another,
-- so the trial balance is zero by construction; balances are always derived, never stored.
CREATE TABLE ledger_entries (
  seq           bigserial PRIMARY KEY,
  id            text NOT NULL UNIQUE,
  idem_key      text NOT NULL UNIQUE,
  type          text NOT NULL,
  order_id      text REFERENCES orders(id),
  creator_id    text REFERENCES creators(id),
  from_account  text NOT NULL,
  to_account    text NOT NULL,
  amount_cents  bigint NOT NULL CHECK (amount_cents > 0),
  signed_cents  bigint NOT NULL,
  ref           text NOT NULL DEFAULT '',
  memo          text NOT NULL DEFAULT '',
  test_mode     boolean NOT NULL DEFAULT false,
  prev_hash     text NOT NULL,
  hash          text NOT NULL UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CHECK (from_account <> to_account)
);
CREATE INDEX ledger_order_idx ON ledger_entries(order_id);
CREATE INDEX ledger_creator_idx ON ledger_entries(creator_id);

CREATE TABLE refunds (
  id                text PRIMARY KEY,
  order_id          text NOT NULL REFERENCES orders(id),
  paypal_refund_id  text NOT NULL UNIQUE,
  amount_cents      bigint NOT NULL CHECK (amount_cents > 0),
  reason            text NOT NULL DEFAULT '',
  test_mode         boolean NOT NULL DEFAULT false,
  created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE payout_batches (
  id                      text PRIMARY KEY,
  sender_batch_id         text NOT NULL UNIQUE,
  paypal_payout_batch_id  text UNIQUE,
  status                  text NOT NULL DEFAULT 'CREATED',
  mode                    text NOT NULL,
  total_cents             bigint NOT NULL DEFAULT 0,
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE payout_items (
  id                     text PRIMARY KEY,
  batch_id               text NOT NULL REFERENCES payout_batches(id),
  creator_id             text NOT NULL REFERENCES creators(id),
  receiver               text NOT NULL,
  amount_cents           bigint NOT NULL CHECK (amount_cents > 0),
  offset_cents           bigint NOT NULL DEFAULT 0,
  paypal_payout_item_id  text UNIQUE,
  status                 text NOT NULL DEFAULT 'CREATED',
  error                  text NOT NULL DEFAULT '',
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payout_items_creator_idx ON payout_items(creator_id);

CREATE TABLE webhook_events (
  id               text PRIMARY KEY,
  paypal_event_id  text NOT NULL UNIQUE,
  event_type       text NOT NULL,
  resource_id      text NOT NULL DEFAULT '',
  verified         boolean NOT NULL DEFAULT false,
  test_mode        boolean NOT NULL DEFAULT false,
  payload          jsonb NOT NULL,
  received_at      timestamptz NOT NULL DEFAULT now(),
  processed_at     timestamptz,
  outcome          text NOT NULL DEFAULT ''
);

CREATE TABLE jobs (
  id          bigserial PRIMARY KEY,
  type        text NOT NULL,
  payload     jsonb NOT NULL DEFAULT '{}'::jsonb,
  dedupe_key  text UNIQUE,
  run_after   timestamptz NOT NULL DEFAULT now(),
  attempts    int NOT NULL DEFAULT 0,
  status      text NOT NULL DEFAULT 'QUEUED' CHECK (status IN ('QUEUED','RUNNING','DONE','FAILED')),
  last_error  text NOT NULL DEFAULT '',
  locked_at   timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_ready_idx ON jobs(status, run_after);

-- State for PayPal test mode (used when no Sandbox credentials are configured).
CREATE TABLE paypal_test_objects (
  id          text PRIMARY KEY,
  kind        text NOT NULL,
  data        jsonb NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Append-only enforcement. An admin reset sets sway.allow_reset = 'on' inside its own transaction.
CREATE FUNCTION sway_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF current_setting('sway.allow_reset', true) = 'on' THEN
    RETURN COALESCE(NEW, OLD);
  END IF;
  IF TG_TABLE_NAME = 'attributions' AND TG_OP = 'UPDATE'
     AND OLD.explanation IS NULL AND NEW.explanation IS NOT NULL
     AND NEW.phi = OLD.phi AND NEW.amount_cents = OLD.amount_cents AND NEW.weight = OLD.weight
     AND NEW.source_id = OLD.source_id AND NEW.creator_id = OLD.creator_id AND NEW.order_id = OLD.order_id THEN
    RETURN NEW; -- the one allowed write: attaching the AI explanation once
  END IF;
  RAISE EXCEPTION 'sway: % is append-only (% rejected)', TG_TABLE_NAME, TG_OP;
END;
$$;

CREATE TRIGGER ledger_append_only BEFORE UPDATE OR DELETE ON ledger_entries
  FOR EACH ROW EXECUTE FUNCTION sway_append_only();
CREATE TRIGGER attributions_append_only BEFORE UPDATE OR DELETE ON attributions
  FOR EACH ROW EXECUTE FUNCTION sway_append_only();
