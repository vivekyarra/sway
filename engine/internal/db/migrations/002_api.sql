-- Stripe-style idempotency keys for mutating API calls: a retried POST replays the stored response.
CREATE TABLE idempotency_keys (
  key           text NOT NULL,
  scope         text NOT NULL,
  request_hash  text NOT NULL,
  status_code   int  NOT NULL DEFAULT 0,  -- 0 = in progress
  response      bytea,
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (key, scope)
);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys(created_at);

-- TRUNCATE bypasses row triggers, so guard it explicitly: only an admin reset may wipe the books.
CREATE TRIGGER ledger_no_truncate BEFORE TRUNCATE ON ledger_entries
  FOR EACH STATEMENT EXECUTE FUNCTION sway_append_only();
CREATE TRIGGER attributions_no_truncate BEFORE TRUNCATE ON attributions
  FOR EACH STATEMENT EXECUTE FUNCTION sway_append_only();

CREATE INDEX orders_decision_idx ON orders(decision_id);
CREATE INDEX ledger_ref_idx ON ledger_entries(ref);
CREATE INDEX ledger_type_idx ON ledger_entries(type);
