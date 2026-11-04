# Sway

> **You swayed the sale, you get paid.**
> When an AI shopping agent buys something because of what a human wrote, Sway measures exactly whose content changed the decision and pays them through PayPal, but only if the buyer keeps the product.

Built for **Build What's Next with PayPal and AI**. MIT licensed. Sandbox only: no real money, and every brand and creator in the demo is fictional.

[![CI](https://github.com/vivekyarra/sway/actions/workflows/ci.yml/badge.svg)](https://github.com/vivekyarra/sway/actions/workflows/ci.yml)
[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/vivekyarra/sway)

![Remove one review and the agent's decision flips](docs/screenshots/flip.png)

---

## The problem

Affiliate marketing pays creators when a human clicks a link and buys. AI shopping agents break that: the agent reads reviews, blogs, forum threads and video transcripts, decides, and checks out, and nobody clicks. The people whose knowledge produced the decision earn nothing.

Existing fixes miss the point. Agent-side affiliate programs pay the *agent*, which biases it. Pay-per-crawl charges for *reading*, whether or not it mattered. **Nobody pays for the content that actually changed what was bought.**

## How Sway works

1. **An agent decides.** A neutral shopping agent (it earns nothing for its choice) reads human-written sources and picks a product.
2. **Sway re-runs the decision.** After checkout, it replays the agent's decision over every combination of sources and computes each source's exact **Shapley value**: its causal share of the purchase.
3. **Commission is split by influence.** The merchant's commission goes to creators in proportion to that causal contribution. Prompt-injection attempts, copied content and sources that argued against the product earn $0.
4. **It vests only if the buyer keeps it.** PayPal refund and dispute webhooks decide. A refund forfeits the commission, a dispute freezes it, and a refund after payout becomes a clawback netted against future earnings.
5. **PayPal Payouts pays the creator**, straight to their PayPal email. Every cent sits in a double-entry, hash-chained, append-only ledger that always balances.

## Try it in 3 minutes

Open the app and press **Judge tour** (bottom right). The eight steps tick themselves off as you go:

1. Ask the agent for headphones (the suggested prompt is pre-filled).
2. In **Why I chose this**, press **Remove & re-run** on Priya Raman's review: the pick flips from Halvo Q7 to Brisane ANC2. That review swayed the sale.
3. Restore it and **Buy with PayPal**. In sandbox mode, log in with the sandbox buyer shown in the tour panel.
4. Watch attribution finish: exact Shapley over every coalition, with a live progress bar and the coalition explorer.
5. **Fast-forward vesting** (merchant page), or wait out the 2-minute demo window.
6. Open Priya's dashboard: the payout and its PayPal batch ID.
7. Refund a second order and watch its commission get forfeited.
8. Open **Merchant → Ledger**: the hash chain verifies and every order balances.

Without any keys, Sway runs end to end on a labelled **PayPal simulator** and a deterministic **offline agent**. Add sandbox keys and a Claude key to switch to the real APIs; the top bar always shows which mode is live.

| Shopper | Creator | Merchant |
|---|---|---|
| ![Order page with Shapley attribution and coalition explorer](docs/screenshots/order.png) | ![Creator dashboard](docs/screenshots/creator.png) | ![Merchant console with AG Grid](docs/screenshots/merchant.png) |

## How attribution works

```mermaid
flowchart LR
    R[Shopper request] --> RET[Retrieval: candidates + top 10 sources]
    RET --> SCR{Screening}
    SCR -- injection / duplicate --> Z[Excluded, earns $0, reason shown]
    SCR -- clean --> D[Decision run: fixed prompt, K=3 samples]
    D --> LOO[Leave-one-out: quick influence + Remove & re-run]
    D --> CAP[PayPal capture]
    CAP --> SH["Exact Shapley over top-6 players: 2^6 coalitions × K runs, cached and parallel"]
    SH --> SPLIT[Commission split, largest remainder, integer cents]
    SPLIT --> L[(Double-entry hash-chained ledger)]
```

- **Value function.** `v(S)` is the probability that the agent picks the purchased product when it sees only the sources in coalition `S`. Each run scores every candidate from 0 to 100; scores pass through a softmax at τ = 10 and are averaged over K = 3 samples ([`agent/agent.go`](engine/internal/agent/agent.go)).
- **Players.** Sources are screened by leave-one-out. The top 6 by |Δ| become players and the rest are background, present in every coalition ([`attribution/attribution.go`](engine/internal/attribution/attribution.go)).
- **Exact Shapley.** `φᵢ = Σ_S |S|!(n−|S|−1)!/n! · [v(S∪i) − v(S)]` over all 64 coalitions, evaluated in parallel. If the call budget (`MAX_ATTRIBUTION_CALLS`) would be exceeded, it falls back to antithetic Monte Carlo and marks the result approximate ([`shapley/shapley.go`](engine/internal/shapley/shapley.go)).
- **Uncertainty.** Per-coalition sample variance is propagated into each φ, so creators see `φ ± σ`. Only influence above a noise floor of 1 point is paid.
- **Influence share.** `share = (v(all) − v(none)) / v(all)`. If the sources moved the decision by less than 5 points, share is 0 and the whole commission goes back to the merchant (try "A case where content doesn't matter").
- **Checked properties.** Efficiency (Σφ = v(N) − v(∅) to ~1e-17), symmetry, null player, the additive game, and Monte Carlo convergence to exact are all unit-tested. Every order page shows the measured efficiency gap.

## How the money works

| Step | Ledger |
|---|---|
| Capture | `COMMISSION_RESERVED`: merchant → order commission (8% by default) |
| Attribution | `ACCRUE_PENDING` per paid source, `SWAY_FEE_PENDING` (15% of the creator pool), `RETURN_TO_MERCHANT` (the part content didn't earn) |
| Return window passes cleanly | `VEST` |
| Refund or chargeback | `FORFEIT` (partial refunds forfeit exactly that fraction, cent-exact) |
| Dispute opened / resolved | `FREEZE` / `UNFREEZE`; a dispute lost to the buyer counts as a full refund |
| Payout | `PAYOUT_SENT`; a failed or returned item becomes `PAYOUT_FAILED_REVERSAL` |
| Refund after payout | Unclaimed item: cancel it, then forfeit. Claimed: `CLAWBACK`, netted later via `CLAWBACK_OFFSET`. Sway never pulls money back from a creator's PayPal |

**Invariant, enforced and tested for every order:** `creator_pool_net + sway_fee + returned_to_merchant = commission`.

The ledger is **double-entry**: every entry moves a positive amount between two accounts, so the trial balance is zero by construction. It is **append-only**: Postgres triggers reject UPDATE, DELETE and TRUNCATE. It is **idempotent**: each entry has a unique key, so a replayed webhook writes nothing. It is **hash-chained**: each entry stores the SHA-256 of the previous one, so any edit to history is detected (verified incrementally on dashboards, fully on `/api/ledger/verify`). And it **cannot overdraw**: an entry that would take a creator, Sway or commission account below zero is refused. Balances are always derived, never stored. See [`ledger/ledger.go`](engine/internal/ledger/ledger.go) and [`payments/payments.go`](engine/internal/payments/payments.go).

## PayPal integration

| API | Used for | Code |
|---|---|---|
| OAuth2 client credentials | A cached token, refreshed a minute early | [`paypal/client.go`](engine/internal/paypal/client.go) |
| Orders v2 create / capture + JS SDK buttons | Checkout of the agent's pick. The price always comes from the database; `PayPal-Request-Id` makes create and capture idempotent; an already-captured order is read back | [`paypal/client.go`](engine/internal/paypal/client.go), [`Checkout.tsx`](web/src/components/Checkout.tsx) |
| Payments: refund capture | Full and partial refunds that forfeit or claw back commission | [`payments/payments.go`](engine/internal/payments/payments.go) |
| Webhooks + verify-webhook-signature | Capture, refund, reversal, dispute and payout events, verified with the exact raw bytes, stored first (deduplicated by event id), then processed by the same handlers as the API responses | [`payments/webhooks.go`](engine/internal/payments/webhooks.go) |
| Payouts: create batch / get batch / cancel unclaimed item | Paying creators by email (outbox pattern: the books commit before PayPal is called), status polling, reversals | [`payments/payouts.go`](engine/internal/payments/payouts.go) |

Subscribed webhook events: `CHECKOUT.ORDER.APPROVED`, `PAYMENT.CAPTURE.COMPLETED`, `PAYMENT.CAPTURE.REFUNDED`, `PAYMENT.CAPTURE.REVERSED`, `CUSTOMER.DISPUTE.CREATED`, `CUSTOMER.DISPUTE.UPDATED`, `CUSTOMER.DISPUTE.RESOLVED`, `PAYMENT.PAYOUTSBATCH.*`, `PAYMENT.PAYOUTS-ITEM.*`.

**Disputes, honestly.** Real sandbox disputes are slow to create, so the real handlers are implemented, and a merchant-only **Simulate dispute** action injects events through the same handler with `simulated = true`. That flag is shown as a SIMULATED badge everywhere. The keyless **PayPal simulator** behaves the same way (idempotent capture, refund limits, payout items that settle as SUCCESS, UNCLAIMED or FAILED) and is labelled in the UI. It never accepts external webhooks.

## AI usage

- **Claude is the shopping agent.** One fixed prompt (`DECISION_PROMPT_V1`) and one model (`claude-haiku-5-5` by default) serve every run, because counterfactuals are only comparable when nothing but the sources changes. Current Claude models reject `temperature` and forced tool use, so output is constrained with **structured outputs** (`output_config.format`, a JSON schema) and validated; an invalid response is retried once. Residual sampling noise is smoothed by averaging K samples ([`agent/claude.go`](engine/internal/agent/claude.go)).
- **Every attribution run is Claude.** The counterfactual re-runs use a content-addressed cache in Postgres (identical coalitions never re-run), singleflight de-duplication of in-flight runs, and one shared concurrency limit.
- **Claude screens for prompt injection** (`is_instruction_to_agent`, evidence), layered over regex rules. Duplicates are caught with 5-word-shingle Jaccard ≥ 0.6 against earlier sources ([`screening/screening.go`](engine/internal/screening/screening.go)).
- **Claude writes creator explanations** ("Your review mattered because…"). Any number it writes is stripped, and the real amounts and percentages are inserted by code.
- **Offline engine.** Without a key, a deterministic evidence-reading agent ([`agent/sim.go`](engine/internal/agent/sim.go)) reads the actual source text, handling negation ("never felt any pressure"), comparatives ("better than") and pronoun carry-over, so the whole pipeline is runnable and testable anywhere.

## Any agent can shop through Sway (MCP)

Sway is also a **Model Context Protocol server** (`POST /mcp`: Streamable HTTP, JSON-RPC 2.0, protocol 2025-06-18). Any MCP agent gets a neutral shopper with receipts:

```bash
claude mcp add --transport http sway https://<your-sway-host>/mcp
```

The tools are `shop_recommend`, `explain_influence`, `counterfactual`, `order_attribution`, `creator_earnings` and `verify_ledger`. `shop_recommend` returns a PayPal checkout link that opens the same decision for the human: **the agent never touches money**, and the people it learned from are paid when the sale sticks. The endpoint is Origin-checked (DNS rebinding) and rate limited ([`mcp/mcp.go`](engine/internal/mcp/mcp.go)).

![Bring your own agent](docs/screenshots/mcp.png)

## Architecture

```mermaid
flowchart TB
    subgraph Browser
      UI[Next.js UI: Shopper · Creator · Merchant<br/>PayPal JS SDK · AG Grid]
    end
    AG[Any MCP agent] -->|/mcp| API
    UI -->|/api, same origin| API
    subgraph "sway (one static Go binary, UI embedded)"
      API[HTTP API<br/>rate limits · Idempotency-Key · CSP · metrics]
      W[Worker<br/>attribution · vesting · payouts · webhooks]
    end
    API --> PG[(Postgres<br/>append-only ledger · job queue · run cache)]
    W --> PG
    PP[PayPal REST: Orders · Refunds · Payouts] <--> API
    PP -->|signed webhooks| API
    W <--> PP
    W --> C[Claude API]
    API --> C
    API --> C3[Channel3 live catalog]
```

- **Go engine** ([`engine/`](engine)): a single static binary with `serve`, `worker`, `migrate`, `seed`, `reset` and `healthcheck`. It uses pgx, a Postgres job queue (`FOR UPDATE SKIP LOCKED`, backoff, stale-lock recovery), graceful shutdown, structured JSON logs and Prometheus `/metrics`.
- **Web UI** ([`web/`](web)): Next.js, statically exported and embedded in the binary. It includes AG Grid Community, the PayPal JS SDK and Tailwind.
- **Production hardening**: request ids, panic recovery, strict CSP/HSTS/frame headers, a CORS allow-list, body limits, per-IP token-bucket rate limits, Stripe-style `Idempotency-Key` replay on every POST, typed errors, one lock order across all money paths (orders, then ledger), and an advisory-locked bootstrap so the web and worker services can start together.

## Tested to breaking point

| Layer | What runs in CI |
|---|---|
| Unit and integration (55 Go tests, race detector) | Shapley properties; 1,000 random allocations and 20,000 random splits balance exactly; ledger idempotency, append-only triggers, tamper detection and overdraft refusal; flip across 10 phrasings; screening; Claude and PayPal contract tests against mock APIs; MCP session; spec scenarios E1, E3–E7, E9–E11 |
| Chaos | 30 rounds and 90 orders of random refunds, disputes, vesting, payouts and webhook replays. After every round: per-creator conservation (`earned = pending + frozen + vested + paid − owed + forfeited + clawed`), no negative bucket, every order balanced, hash chain intact |
| Concurrency storm | 10 parallel captures per order with racing refunds, disputes and payouts. This test found a real payout/dispute race and a lock-order deadlock, both fixed |
| Browser E2E (Playwright, real binary + Postgres) | The full 8-step judge tour; E2 flip; E4/E6/E7 refunds and disputes; E8 screening; E9; E10 idempotent replay; MCP agent → checkout handoff; no horizontal scroll on phones; an axe-core WCAG 2.1 AA audit of every surface (no serious or critical violations) |
| Load ([`cmd/swayload`](engine/cmd/swayload)) | Concurrent shoppers through decide → counterfactual → order → capture → refund, then settle and assert zero 5xx, every order balanced and the ledger verified |

Load test on a 4-core box with local Postgres (offline agent), **128 concurrent shoppers, 2,000 purchases**:

```text
endpoint         calls       p50       p95       p99     5xx
decide            2000     466ms     669ms     695ms       0
rerun             2000     376ms     479ms     575ms       0
create_order      2000     259ms     362ms     391ms       0
capture           2000     501ms     661ms     708ms       0
refund             577     301ms     417ms     463ms       0
orders balanced: 2000/2000, attributions done: 2000/2000
ledger: 14396 entries, hash chain intact, trial balance zero
```

Every one of those 2,000 purchases also ran a full exact-Shapley attribution in the background.

## Run it locally

**Docker (one command, no keys needed):**

```bash
docker compose up --build
```

Then open http://localhost:8080.

**Native:** Go 1.25+, Node 20+ and Postgres 16.

```bash
createdb sway
```

```bash
cd web && npm ci && npm run build && npm run embed && cd ..
```

```bash
cd engine && DATABASE_URL=postgres://localhost:5432/sway?sslmode=disable go run ./cmd/sway serve
```

On first start, Sway migrates the schema and installs the demo world: a merchant, 10 products in three categories, 8 creators, 12 sources, and a 12-order seeded history covering vested, paid, refunded, clawed-back and disputed states. For UI development, run `npm run dev` in `web/` (it proxies `/api` to the engine on :8080).

**Tests:**

```bash
cd engine && go test -race ./...
```

```bash
cd engine && go run ./cmd/swayload -shoppers 32 -orders 400
```

```bash
cd web && npx playwright test
```

### Switching on the real APIs

Copy `.env.example` to `.env`. Every key is optional.

1. **PayPal sandbox.** In the [developer dashboard](https://developer.paypal.com/dashboard/), create a REST app on a sandbox business account (Larkline Audio) and set `PAYPAL_CLIENT_ID` and `PAYPAL_CLIENT_SECRET`. Create one sandbox personal account for the judge buyer (`PAYPAL_SANDBOX_BUYER_EMAIL` / `_PASSWORD`, shown in the judge tour) and one per creator, and map creators to them with `CREATOR_PAYPAL_EMAILS=c_priya=priya@…,c_maya=maya@…` so payouts land in accounts you can log into. Register a webhook at `https://<host>/api/webhooks/paypal` for the events listed above and set `PAYPAL_WEBHOOK_ID`.
2. **Claude.** Set `ANTHROPIC_API_KEY` (default model `claude-haiku-5-5`, configurable with `DECISION_MODEL`).
3. **Channel3.** Set `CHANNEL3_API_KEY` and `CATALOG_MODE=live`. Requests outside the demo categories then pull real products. The demo video uses only the invented catalog.

Locally, webhooks need a tunnel. You can also skip them: capture and refund API responses drive the same idempotent handlers, and payout status is polled.

## Deploy on Render

[`render.yaml`](render.yaml) is a Blueprint for a web service (API + UI), a background worker and managed Postgres. Use **New → Blueprint**, pick this repo, and fill the secrets. The health check is `/api/health`, which reports the database, PayPal reachability, LLM mode and the job queue.

## Honest limitations

- Sandbox only. Disputes are real handlers plus clearly labelled simulations. The demo vesting window is 2 minutes (30 days in production).
- Attribution cost grows as 2ⁿ × K. Exact mode caps at 6 players (≤ 192 runs, fewer with the cache) and switches to Monte Carlo past `MAX_ATTRIBUTION_CALLS`.
- Without an Anthropic key the agent is the deterministic offline engine. It reads the source text with a lexical model, which is convincing on the seeded scenario but is not Claude.
- Sources with negative φ (for example, a review warning against a product that was later returned) earn nothing in v1.

## Roadmap and business model

- **Model:** Sway keeps 15% of the creator commission that vests. Merchants already pay affiliates 5–15%; Sway lets them pay only for influence on kept sales.
- **Wedge:** start as a neutral agent and the MCP server (shipped), then an SDK so any agent can report its decisions and sources to Sway, plus merchant plugins.
- **Moat:** the content → purchase → return graph. More creators means better agent evidence, which brings more merchants and more creator income.
- **Next:** real merchant onboarding (PayPal partner integration and platform fees), Log in with PayPal for creator verification, Invoicing for Sway's fee, multi-currency, tax reporting, a distilled scoring model to cut attribution cost, and an audit API for regulators and disclosure compliance.

## Judging criteria map

| Criterion | Where to look |
|---|---|
| Technological implementation | Exact Shapley over parallel LLM re-runs with caching, variance and a Monte Carlo fallback; the full PayPal lifecycle (orders, capture, refunds, verified webhooks, payouts with polling, cancel-unclaimed, idempotency); a double-entry hash-chained ledger; chaos, concurrency and load tests |
| Design | Three role surfaces in one visual system, a self-ticking judge tour, the "Why I chose this" panel with the flip moment, the coalition explorer, AG Grid dashboards, phone layouts, a WCAG 2.1 AA audit in CI |
| Potential impact | Creators losing income to AI agents get paid per purchase they influenced; merchants pay only for influence on kept sales; shoppers see why the agent chose |
| Innovation | Payment for **causal influence on kept purchases**, measured counterfactually, with manipulation earning $0, plus an MCP server that makes this a layer under any agent |
| Presentation | The 3-minute judge tour, this README, [`devpost/`](devpost) (submission text and video script) |

## Tools

| Tool | How Sway uses it |
|---|---|
| Claude (Anthropic API, Go SDK) | Shopping-agent decisions; every counterfactual re-run for attribution; prompt-injection screening; creator explanations |
| PayPal Orders v2 + JS SDK | Checkout and capture of agent-chosen purchases |
| PayPal Refunds | Refunds that forfeit or claw back commission |
| PayPal Webhooks | Capture, refund, dispute and payout events, signature-verified, driving vesting |
| PayPal Payouts | Paying creators by email, with status polling and cancellation of unclaimed items |
| Channel3 | Live product catalog mode |
| AG Grid Community | Creator and merchant dashboards (influence events, content performance, orders, payouts, ledger, webhooks) |
| Render | Blueprint for the web service, background worker and Postgres |

## License

[MIT](LICENSE)
