# Tools used

| Tool | How Sway uses it |
|---|---|
| Claude (Anthropic API, `claude-haiku-5-5` by default) | Shopping-agent decisions; every counterfactual re-run for Shapley attribution; prompt-injection screening; plain-language creator explanations (numbers inserted by code) |
| PayPal Orders v2 + JS SDK | Checkout and capture of agent-chosen purchases, with the price taken from the database and idempotent via PayPal-Request-Id |
| PayPal Refunds | Full and partial refunds that forfeit or claw back commission |
| PayPal Webhooks | Capture, refund, reversal, dispute and payout events, signature-verified and deduplicated, driving vesting |
| PayPal Payouts | Paying creators by email; status polling, reversals, cancellation of unclaimed items |
| Channel3 | Live product catalog mode (`/v1/search`) |
| AG Grid Community | Creator and merchant dashboards: influence events, content performance with sparklines, orders, payouts, ledger, webhooks |
| Render | Blueprint for the web service, background worker and managed Postgres |
| Model Context Protocol | Sway as a tool server so any AI agent can shop through it with attribution |
