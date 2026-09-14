# Paid LLM Inference Gateway

This is a single Go binary that fronts an Ollama instance with invite-only API
keys, prepaid USDC balances, usage metering, and per-key abuse controls.

x402 is used only to fund a balance. Inference debits happen off-chain in
Postgres using integer micro-USDC values.

## Requirements

- Go 1.25 or newer
- Postgres 14 or newer
- Ollama running locally or at a configured URL
- A receiving EVM address for USDC top-ups
- A facilitator reachable by the gateway. The default is Coinbase CDP's x402
  facilitator and requires `FACILITATOR_AUTHORIZATION` from a CDP bearer token.

## Build

```sh
go build -o gateway ./cmd/gateway
```

Apply the schema once:

```sh
psql "$POSTGRES_URL" -f migrations/001_init.sql
```

Copy `config.example.env` to a protected location, replace the placeholder
`PAY_TO` and the
database credentials, then create an invite:

```sh
./gateway user add --config=/etc/payed-llm-inference/gateway.env --label=demo
```

The command prints the API key once. Store it in the client securely. The
gateway stores only its SHA-256 digest.

Start the service on Base Sepolia:

```sh
./gateway serve --config=/etc/payed-llm-inference/gateway.env
```

`NETWORK=base-sepolia` is the safe default. Use `NETWORK=base` only after
testing the complete flow with test USDC.

## API

All endpoints except `/healthz` use `Authorization: Bearer <api-key>`.
`X-API-Key` is also accepted.

Request a top-up. A first request returns an x402 `402 Payment Required`
response containing the exact requested amount. An x402-aware client pays and
retries with `X-PAYMENT`; the gateway verifies, settles, and credits the
balance.

```sh
curl -i http://localhost:8080/v1/topups \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"amount":"1.00"}'
```

Inspect the current balance:

```sh
curl http://localhost:8080/v1/users/me -H "Authorization: Bearer $API_KEY"
```

Run generation through either endpoint:

```sh
curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"Explain x402 in one paragraph"}],"stream":true}'
```

The gateway pins requests to the configured Ollama model and enforces the
safety `num_predict` ceiling. Streaming responses retain Ollama's NDJSON
format. If the predictive balance check reaches the reload threshold, the
stream ends cleanly with a final event containing `partial: true` and
`top_up_required: true`; the client can top up and retry with its conversation
history.

## Admin commands

```sh
./gateway user list --config=FILE
./gateway user revoke --config=FILE --key=API_KEY
./gateway user limit --config=FILE --key=API_KEY --rate=60 --concurrency=1
./gateway topup reconcile --config=FILE --key=API_KEY --amount-micro=1000000 \
  --transaction=0x... --payer=0x... --network=base-sepolia
```

The reconcile command is an operator recovery path if settlement succeeded but
the database was unavailable before crediting. Confirm the transaction on-chain
before using it.

## Deployment

`deploy/gateway.service` runs the same binary under systemd with journald
logging, restart-on-failure, and boot startup. Tunnels and reverse proxies are
intentionally outside the binary. Copy the unit to `/etc/systemd/system/`,
adjust paths and the service user, then enable it with `systemctl enable --now`.

## Structure

- `internal/payment`: x402 requirements, facilitator verification, and settlement
- `internal/ledger`: Postgres balance and audit transactions
- `internal/meter`: checkpoint estimates and final usage debit
- `internal/ollama`: bounded request preparation and NDJSON streaming
- `internal/guardrails`: per-key rate and concurrency controls
- `internal/httpapi`: HTTP contract and request orchestration
- `cmd/gateway`: server and admin CLI entry point

See `engineering-choices.md` for behavior where the planning documents leave
an implementation detail open.
