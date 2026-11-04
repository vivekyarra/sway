# Sway: you swayed the sale, you get paid

## Inspiration
AI agents are starting to shop for us. They read reviews, blogs, forum answers and video transcripts, then buy. Nobody clicks an affiliate link, so the people whose knowledge made the decision earn nothing, just as AI answers are already draining their traffic. The fixes on offer pay the wrong party: agent-side commissions pay the agent (and bias it), and pay-per-crawl pays for reading whether or not it mattered.

## What it does
Sway pays the humans whose writing actually changed what an AI agent bought, and only if the buyer keeps it.

1. A neutral shopping agent (Claude) reads human-written sources and picks a product. It earns nothing for its choice.
2. After the PayPal capture, Sway replays the agent's decision over every combination of sources and computes exact **Shapley values**: each source's causal share of the purchase.
3. The merchant's commission is split by that causal contribution. Content that tries to command AI agents is blocked, copied content is caught, and sources that argued against the product earn nothing.
4. Commission **vests only if the purchase survives**: PayPal refund and dispute webhooks decide. Returns forfeit it, disputes freeze it, and refunds after payout become clawbacks netted against future earnings.
5. Vested money goes to creators with **PayPal Payouts**.

The demo moment: the agent picks the Halvo Q7. Click **Remove** on one travel writer's review and the agent picks the Brisane ANC2 instead. That review swayed the sale, and after vesting a payout lands in her PayPal.

Sway is also an **MCP server**, so any AI agent (Claude Code, Claude Desktop, others) can shop through Sway. It gets a neutral pick with its human sources and a PayPal checkout link for the person. The agent never touches money.

## How we built it
- **Go engine** (one static binary with the UI embedded): attribution engine, double-entry hash-chained ledger, PayPal integration, Postgres job queue, MCP server.
- **Claude** via the official Go SDK with structured outputs: one fixed prompt and model for every counterfactual run, K-sample averaging, a content-addressed run cache, and singleflight de-duplication.
- **PayPal**: Orders v2 + JS SDK buttons, capture refunds, signature-verified webhooks, Payouts with status polling and cancellation of unclaimed items. `PayPal-Request-Id` idempotency everywhere.
- **Next.js + AG Grid** for the Shopper, Creator and Merchant surfaces; **Render** Blueprint for web, worker and Postgres; **Channel3** for live-catalog mode.

## Challenges
- Making counterfactuals trustworthy: identical bytes for identical coalitions, caching, variance propagated into every φ, a noise floor, and a measured Shapley efficiency gap (~1e-17) shown on every order.
- Money correctness under concurrency. Our concurrency stress test caught a real race (a payout spending money a dispute had just frozen) and a lock-order deadlock. We fixed both with one lock order across all money paths, plus a ledger guard that refuses any overdraft.

## Accomplishments
- Exact Shapley attribution over LLM re-runs, end to end with real payment rails.
- Ledger invariants hold under chaos testing (30 rounds of random refunds, disputes, payouts and webhook replays) and a 2,000-purchase load test with 128 concurrent shoppers: zero errors and every order balanced to the cent.
- A self-ticking 3-minute judge tour.

## What's next
Real merchant onboarding with PayPal partner flows, Log in with PayPal for creators, Invoicing for Sway's fee, an SDK so any agent can report its decisions to Sway, and a distilled scoring model to cut attribution cost.

## Built with
go · postgresql · claude · anthropic · paypal · paypal-payouts · nextjs · typescript · tailwindcss · ag-grid · mcp · channel3 · render · docker · playwright
