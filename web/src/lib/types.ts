export type Product = {
  id: string;
  name: string;
  brand: string;
  category: string;
  price_cents: number;
  tagline: string;
  image_url?: string;
  source?: string;
  specs: Record<string, string | number | boolean>;
};

export type Claim = { source_id: string; product_id: string; aspect: string; strength: number; sentence: string };

export type Source = {
  id: string;
  creator_id: string;
  creator_name: string;
  type: "review" | "blog" | "forum" | "transcript";
  title: string;
  body: string;
  published_at: string;
  status: string;
  reason?: string;
  product_ids: string[];
  claims?: Claim[] | null;
};

export type Influence = {
  v_full: number;
  deltas: Record<string, number>;
  picks_without: Record<string, string>;
  v_none: number;
  pick_no_sources: string;
};

export type Decision = {
  id: string;
  request: string;
  budget_cents: number;
  category: string;
  candidates: Product[];
  sources: Source[];
  ignored_sources: Source[] | null;
  chosen: Product;
  rationale: string;
  relied_on_source_ids: string[] | null;
  probabilities: Record<string, number>;
  engine: string;
  model: string;
  prompt_version: string;
  status: string;
  influence?: Influence;
};

export type Rerun = {
  decision_id: string;
  excluded_source_ids: string[];
  chosen: Product;
  rationale: string;
  probabilities: Record<string, number>;
  flipped: boolean;
  agreement: string;
};

export type LedgerEntry = {
  seq: number;
  id: string;
  idem_key: string;
  type: string;
  order_id?: string;
  creator_id?: string;
  from_account: string;
  to_account: string;
  amount_cents: number;
  signed_cents: number;
  ref?: string;
  memo?: string;
  test_mode: boolean;
  prev_hash: string;
  hash: string;
  created_at: string;
};

export type Coalition = { mask: number; members: string[] | null; v: number; std: number; pick: string };
export type Player = {
  source_id: string;
  creator_id: string;
  creator_name: string;
  phi: number;
  phi_stddev: number;
  loo_delta: number;
  pick_without: string;
  role: string;
};

export type AttributionJob = {
  status: "QUEUED" | "RUNNING" | "DONE" | "FAILED";
  phase: string;
  progress: number;
  total_runs: number;
  cache_hits: number;
  method: string;
  approximate: boolean;
  v_full: number | null;
  v_none: number | null;
  share: number | null;
  efficiency_gap: number | null;
  error?: string;
  coalitions?: {
    players: string[];
    background: string[] | null;
    coalitions: Coalition[];
    interactions: number[][] | null;
    v_all: number;
    v_none: number;
    v_background: number;
  } | null;
};

export type AttributionRow = {
  source_id: string;
  source_title: string;
  source_type: string;
  creator_id: string;
  creator_name: string;
  avatar_hue: number;
  phi: number;
  phi_stddev: number;
  loo_delta: number;
  weight: number;
  amount_cents: number;
  role: string;
  explanation: string | null;
  status: string;
  reason: string;
};

export type Split = {
  commission_cents: number;
  creator_pool_net_cents: number;
  sway_fee_cents: number;
  returned_to_merchant_cents: number;
  undistributed_cents: number;
  forfeited_cents: number;
  clawed_back_cents: number;
  balanced: boolean;
};

export type Order = {
  id: string;
  decision_id: string | null;
  product_id: string;
  product_name: string;
  product_brand: string;
  request: string;
  rationale: string;
  amount_cents: number;
  refunded_cents: number;
  status: string;
  dispute_status: string;
  dispute_test: boolean;
  payment_mode: "sandbox" | "test" | "historical";
  historical: boolean;
  paypal_order_id: string | null;
  paypal_capture_id: string | null;
  captured_at: string | null;
  vests_at: string | null;
  vested_at: string | null;
  attribution: AttributionJob | null;
  attributions: AttributionRow[] | null;
  split: Split;
  refunds: { paypal_refund_id: string; amount_cents: number; reason: string; test_mode: boolean; created_at: string }[] | null;
  ledger: LedgerEntry[] | null;
};

export type Balances = {
  pending_cents: number;
  frozen_cents: number;
  vested_cents: number;
  paid_cents: number;
  forfeited_cents: number;
  clawed_back_cents: number;
  owed_cents: number;
  earned_cents: number;
};

export type PayoutItem = {
  id: string;
  batch_id: string;
  creator_id: string;
  creator_name: string;
  receiver: string;
  amount_cents: number;
  offset_cents: number;
  status: string;
  paypal_payout_item_id: string | null;
  paypal_payout_batch_id: string | null;
  mode: string;
  error?: string;
  created_at: string;
};

export type CreatorEvent = {
  order_id: string;
  date: string;
  product_name: string;
  source_id: string;
  source_title: string;
  phi: number;
  phi_stddev: number;
  contribution_pct: number;
  amount_cents: number;
  status: string;
  reason: string;
  explanation: string | null;
  historical: boolean;
  test_mode: boolean;
};

export type Creator = {
  id: string;
  display_name: string;
  paypal_email: string;
  verified: boolean;
  bio: string;
  avatar_hue: number;
  balances: Balances;
  purchases_swayed: number;
  headline: string;
  events: CreatorEvent[] | null;
  sources: {
    id: string;
    title: string;
    type: string;
    published_at: string;
    status: string;
    status_reason: string;
    purchases_influenced: number;
    purchases_returned: number;
    return_rate: number;
    earned_cents: number;
  }[] | null;
  payouts: PayoutItem[] | null;
};

export type CreatorSummary = {
  id: string;
  display_name: string;
  bio: string;
  avatar_hue: number;
  sources: number;
  flagged_sources: number;
  earned_cents: number;
  paid_cents: number;
};

export type Merchant = {
  id: string;
  name: string;
  commission_rate_bps: number;
  sway_fee_bps: number;
  vesting_window_seconds: number;
  kpis: {
    agent_revenue_cents: number;
    orders: number;
    commission_cents: number;
    commission_paid_cents: number;
    commission_returned_cents: number;
    commission_forfeited_cents: number;
    commission_pending_cents: number;
    sway_fee_cents: number;
    influenced_return_rate: number;
    avg_influence_share: number;
  };
  content: {
    source_id: string;
    title: string;
    type: string;
    creator_id: string;
    creator_name: string;
    status: string;
    status_reason: string;
    purchases_influenced: number;
    avg_contribution: number;
    revenue_influenced_cents: number;
    return_rate: number;
    commission_vested_cents: number;
    commission_earned_cents: number;
    spark: number[] | null;
  }[];
  orders: {
    id: string;
    product_name: string;
    amount_cents: number;
    refunded_cents: number;
    status: string;
    dispute_status: string;
    dispute_test: boolean;
    attribution_status: string;
    share: number | null;
    creator_pool_cents: number;
    fee_cents: number;
    returned_to_merchant_cents: number;
    commission_cents: number;
    balanced: boolean;
    payment_mode: string;
    historical: boolean;
    paypal_capture_id: string | null;
    captured_at: string | null;
    vests_at: string | null;
    vested_at: string | null;
  }[];
  payouts: PayoutItem[] | null;
  batches: {
    id: string;
    sender_batch_id: string;
    paypal_payout_batch_id: string | null;
    status: string;
    mode: string;
    total_cents: number;
    items: number;
    created_at: string;
  }[] | null;
  ledger: LedgerEntry[] | null;
  webhooks: {
    paypal_event_id: string;
    event_type: string;
    resource_id: string;
    verified: boolean;
    test_mode: boolean;
    received_at: string;
    processed_at: string | null;
    outcome: string;
  }[] | null;
  verification: { ok: boolean; entries: number; head: string; broken_at_seq?: number; problem?: string; trial_balance_zero: boolean };
  daily: { day: string; revenue_cents: number; commission_cents: number }[];
};

export type AppConfig = {
  version: string;
  test_tools: boolean;
  catalog_mode: string;
  paypal: { mode: "sandbox" | "test"; client_id?: string; buyer_email?: string; buyer_password?: string };
  llm: { mode: "claude" | "gemini" | "openai" | "local"; model: string; prompt_version: string };
  vesting_window_seconds: number;
  commission_rate_bps: number;
  sway_fee_bps: number;
  min_payout_cents: number;
  attribution: { samples: number; players: number; max_calls: number; tau: number };
  categories: { key: string; label: string; suggested_prompt: string }[];
};
