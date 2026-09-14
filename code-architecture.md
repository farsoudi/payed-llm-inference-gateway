# Code Architecture
*Auto-Generated with GPT-5.6 Luna*

This document explains how the current Go implementation is organized, how a
request moves through it, and how the individual files cooperate. It is meant
to be read alongside `PLANNING.md`, `model.md`, `README.md`, and
`engineering-choices.md`.

The short version is:

```text
HTTP client
    |
    v
cmd/gateway -> internal/httpapi
                    |
        +-----------+-----------+----------------+
        |                       |                |
      auth                 guardrails         meter
        |                       |                |
        +----------+------------+        +-------+-------+
                   |                     |               |
                ledger              Ollama client     money
                   |                     |               |
                Postgres             Ollama :11434     exact USDC math
```

The x402 funding path is a second branch of the HTTP layer:

```text
Client -> gateway /v1/topups -> x402 facilitator -> Base USDC settlement
                                      |
                                      v
                              Postgres top-up ledger
```

The gateway is one process and one binary. It does not run Ollama, Postgres,
or a tunnel. It connects to those services using configuration.

## 1. Repository Map

### Entry Point

`cmd/gateway/main.go` is the executable package. It contains:

- Root command dispatch.
- The long-running HTTP server startup path.
- User administration commands.
- Top-up reconciliation command.
- Context timeouts for database operations.
- Graceful shutdown handling.

The executable can be built with:

```sh
go build -o gateway ./cmd/gateway
```

### Runtime Packages

The `internal/` directory prevents external Go modules from importing these
implementation packages directly.

| Package | File | Responsibility |
| --- | --- | --- |
| `config` | `internal/config/config.go` | Defaults, config-file parsing, environment overrides, validation |
| `auth` | `internal/auth/auth.go` | API-key generation, hashing, and HTTP extraction |
| `domain` | `internal/domain/domain.go` | Shared data structures and monetary unit constant |
| `money` | `internal/money/money.go` | Exact USDC parsing, formatting, and multiplication |
| `ledger` | `internal/ledger/store.go` | Storage interface and storage errors |
| `ledger` | `internal/ledger/postgres.go` | Postgres pool and accounting transactions |
| `payment` | `internal/payment/payment.go` | x402 requirement, verification, settlement, and response headers |
| `ollama` | `internal/ollama/client.go` | Complete reverse proxy, request preparation, and native/SSE stream readers |
| `meter` | `internal/meter/meter.go` | Balance cache, upfront reservations, usage observation, and final debit |
| `guardrails` | `internal/guardrails/guardrails.go` | Per-key rate and concurrency limiting |
| `guardrails` | `internal/guardrails/failures.go` | Process-local repeated-failure monitoring |
| `httpapi` | `internal/httpapi/server.go` | Routes, authentication, orchestration, response handling |

### Operational Files

- `migrations/001_init.sql`: schema applied manually by the operator.
- `config.example.env`: complete configuration example.
- `deploy/gateway.service`: systemd unit for the same binary.
- `README.md`: build, setup, API, and operator instructions.
- `engineering-choices.md`: decisions and known deviations from the planning
  documents.
- `go.mod` and `go.sum`: Go module metadata.

## 2. Process Startup

The process starts in `main()`.

### Command Dispatch

`main()` examines `os.Args[1]`:

- `serve` calls `serve(args)`.
- `user` calls `userCommand(args)`.
- `topup` calls `topupCommand(args)`.
- An argument beginning with `-`, such as `--config`, is treated as the
  shorthand server invocation.
- Anything else prints usage and exits with status 2.

This means both forms are supported:

```sh
./gateway serve --config=/path/gateway.env
./gateway --config=/path/gateway.env
```

### Server Startup

`serve()` performs these steps in order:

1. Create a standard-library `flag.FlagSet`.
2. Read the optional `--config` path.
3. Call `config.Load()`.
4. Call `Config.Validate()`.
5. Open a Postgres pool with a 15-second startup context.
6. Construct `httpapi.Server` with `httpapi.New()`.
7. Construct an `http.Server` with:
   - The configured listening address.
   - The application handler.
   - A 10-second read-header timeout.
   - A two-minute idle timeout.
8. Start `ListenAndServe()` in a goroutine.
9. Wait for SIGINT or SIGTERM.
10. Give active HTTP requests 15 seconds to shut down cleanly.
11. Close the Postgres pool when `serve()` returns.

The only service this process actively starts is the HTTP listener. Ollama must
already be serving, normally on `127.0.0.1:11434`.

## 3. Configuration

`internal/config/config.go` defines the `Config` struct. Configuration values
are loaded in this precedence order:

1. Hard-coded safe defaults from `Defaults()`.
2. The optional file passed through `--config`.
3. Environment variables.

Environment variables override file values. The file parser accepts simple
lines in this form:

```text
KEY=value
```

Blank lines and lines beginning with `#` are ignored. Values may be surrounded
by single or double quotes. The parser is intentionally not a full dotenv
implementation.

### Important Defaults

- Listen address: `:8080`.
- Ollama URL: `http://127.0.0.1:11434`.
- Ollama model: `qwen2.5`.
- Network: `base-sepolia`.
- Facilitator: Coinbase CDP's x402 endpoint.
- Price: `5` micro-USDC per generated token.
- Minimum top-up: `500000` micro-USDC, or `$0.50`.
- Safety ceiling: `16384` generated tokens.
- Request timeout: `10m`.
- Gateway-owned and metered JSON request body limit: `1 MiB`. Transparent
  Ollama routes preserve arbitrary request bodies, including blob uploads.
- New-user rate limit: `60` requests per minute.
- New-user concurrency limit: `1` request.

### Validation

The server validates:

- `POSTGRES_URL` is present.
- `PAY_TO` is a non-zero 20-byte hexadecimal EVM address.
- Ollama URL and model are present.
- Price, minimum top-up, safety ceiling, timeouts, and limits are positive.
- Network is either `base-sepolia` or `base`.
- Facilitator URL is HTTP or HTTPS.
- Mainnet facilitator URLs use HTTPS.

The admin commands only require a Postgres URL because they do not start the
HTTP/payment server.

## 4. Shared Domain Types

`internal/domain/domain.go` is intentionally small. It contains data passed
between modules without making those modules depend on one another's private
implementation details.

### `User`

`User` represents the database-backed account:

- Database ID.
- Human-readable label.
- Balance in micro-USDC.
- Requests-per-minute limit.
- Concurrent-request limit.
- Revocation state.
- Creation time.

The API key itself is not included in `User`. Only its hash is persisted and
used for lookup.

### `TopUp`

`TopUp` carries the data needed to record a settled payment:

- API-key hash.
- Amount in micro-USDC.
- Blockchain transaction hash.
- Payer address.
- Network identifier.

### `Debit`

`Debit` carries usage information into the ledger:

- API-key hash.
- Prompt token count.
- Completion token count.
- Cost in micro-USDC.
- Request latency.
- Whether the response was partial.

## 5. API-Key Authentication

`internal/auth/auth.go` has three important operations.

### Generating a Key

`NewKey()` reads 32 random bytes from `crypto/rand`, encodes them using raw
URL-safe Base64, and prefixes the result with `pli_`.

The plaintext key is printed by `gateway user add` once. The gateway never
needs to recover it from the database.

### Hashing a Key

`Hash()` computes SHA-256 and returns a raw URL-safe Base64 digest. The digest
is the `users.key_hash` lookup value.

The authentication design is therefore:

```text
request key -> SHA-256 -> database lookup
```

### Extracting a Key

`FromRequest()` checks:

1. `Authorization: Bearer <key>`.
2. `X-API-Key: <key>` if the Authorization header did not contain a key.

The HTTP server hashes the extracted value and calls `Store.GetUser()`.
Unknown and revoked keys receive HTTP 401. The raw key is not included in
structured logs.

## 6. Ledger Abstraction

`internal/ledger/store.go` defines the `Store` interface. The HTTP and metering
layers depend on this interface rather than directly on pgx. This is the main
testability seam for replacing Postgres with a fake in unit tests.

The interface supports:

- Creating users.
- Looking up users.
- Listing users.
- Revoking users.
- Changing limits.
- Crediting top-ups.
- Recording debits.
- Closing the store.

The package also defines sentinel errors:

- `ErrNotFound`.
- `ErrInsufficientFunds`.
- `ErrRevoked`.
- `ErrConflict` for conflicting reuse of a transaction hash.

Callers use `errors.Is()` so storage wrapping does not destroy the meaning of
these errors.

## 7. Postgres Implementation

`internal/ledger/postgres.go` implements `Store` with `pgxpool.Pool`.

### Opening The Pool

`ledger.Open()` creates a pool from the configured connection string and calls
`Ping()` before returning. A startup failure prevents the gateway from serving
requests.

### User Operations

`CreateUser()` inserts the key digest and limits, then returns the created
record.

`GetUser()` selects a record by digest. It maps no row to `ErrNotFound` and
maps a revoked record to `ErrRevoked`.

`ListUsers()` returns all users ordered by database ID. It intentionally does
not expose key hashes in the JSON representation because `User` has no key-hash
field.

`DeleteUser()` is a soft revoke. It sets `revoked=true` and records
`revoked_at`; it does not delete balances or audit history.

`SetLimits()` updates rate and concurrency values only for a non-revoked user.

### Top-Up Transaction

`CreditTopUp()` opens a transaction and performs:

1. Insert a top-up row with `ON CONFLICT (transaction_hash) DO NOTHING`.
2. If the transaction already exists, load its original metadata.
3. Reject the retry with `ErrConflict` if key, amount, payer, or network differ.
4. For an identical retry, return the current balance with `inserted=false`.
5. For a new payment, increment the user's balance.
6. Commit both the top-up record and balance increment together.

This gives a retry-safe result for a client that does not know whether its
previous request committed.

The chain settlement and the Postgres transaction cannot be one atomic
transaction. If settlement succeeds but the database becomes unavailable, the
operator can verify the transaction and use `gateway topup reconcile`.

### Debit Transaction

`Debit()` opens a transaction and performs one conditional update:

```sql
UPDATE users
SET balance_micro_usdc = balance_micro_usdc - $cost
WHERE key_hash = $key
  AND revoked = false
  AND balance_micro_usdc >= $cost
RETURNING balance_micro_usdc
```

If this update returns no row, the code distinguishes unknown, revoked, and
insufficient-funds cases. If it succeeds, it inserts the matching
`request_ledger` row before committing.

The balance update and usage record therefore succeed or fail together.

## 8. Exact Money Handling

`internal/money/money.go` prevents floating-point accounting.

### Units

The application stores one USDC as `1_000_000` units. For example:

```text
"1"       -> 1,000,000
"0.50"    -> 500,000
"0.000001"-> 1
```

`ParseUSDC()` accepts at most six fractional digits, rejects negative values,
rejects malformed decimals, and checks integer overflow.

`FormatUSDC()` converts micro-USDC back to a fixed six-decimal string for API
responses and x402 requirement construction.

`MultiplyTokens()` calculates `token_count * price_micro_usdc` with overflow
protection.

The payment and ledger layers never use `float64` for account balances.

## 9. Guardrails

### Request Limiter

`internal/guardrails/guardrails.go` implements a process-local controller.

Each key maps to a state containing:

- Its current limits.
- The start of the one-minute rate window.
- Requests used in that window.
- Active request count.

`Acquire()` loops until both conditions are true:

```text
active requests < concurrency limit
requests this window < rate limit
```

It returns a release function. The HTTP handler defers that function so the
active count is reduced when inference exits, including error paths.

When concurrency is the blocking condition, the controller checks again after
50 milliseconds. When the rate window is the blocking condition, it waits for
the window to reset. A canceled request stops waiting.

The state is keyed by the API-key hash, not wallet address or source IP.

### Failure Monitor

`internal/guardrails/failures.go` tracks one-minute failure windows by source
address. It is used for malformed and unauthenticated requests. Once a source
reaches the warning threshold, structured warnings are logged.

This is monitoring, not a replacement for the authenticated per-key rate
limiter. It is deliberately process-local and intended to feed an operator's
log/alerting system.

## 10. HTTP Server Composition

`internal/httpapi/server.go` owns the application-level orchestration.

### Construction

`httpapi.New()` constructs:

- One `meter.BalanceCache`.
- One `meter.Meter` using the configured `Store`.
- One guardrail controller.
- One failure monitor.
- One Ollama client with the configured URL, model, and timeout.
- One x402 payment service.
- The default structured logger.

The returned `Server` is a composition root: it wires concrete implementations
together while the lower-level packages remain independently understandable.

### Routes

`Server.Handler()` registers the gateway-owned routes and one wildcard fallback:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/healthz` | Unauthenticated health response |
| `GET` | `/v1/users/me` | Authenticated account and balance lookup |
| `POST` | `/v1/topups` | Dynamic x402-funded balance top-up |
| `ANY` | `/api/*` and `/v1/*` | Authenticated Ollama API proxy; known compute paths add metering |
| `ANY` | `/` | Authenticated Ollama root/status proxy |

The route uses Go's `http.ServeMux`; no third-party router is needed. The
wildcard handler does not enumerate Ollama routes, so newly added Ollama APIs
are available without gateway code changes. `/v1/users/me` and `/v1/topups`
remain gateway-owned and take precedence over the wildcard.

### Request Logging Wrapper

`requestLogging()` wraps the response writer to record method, path, status, and
duration. `contextWriter` preserves `http.Flusher`, which is necessary for
streaming clients to receive output while Ollama is still generating.

The wrapper is outside the route mux, so every route receives the same logging
behavior.

## 11. Authentication In The HTTP Layer

Every authenticated handler begins with `s.user(w, r)`:

1. Extract the API key using `auth.FromRequest()`.
2. Hash it.
3. Look up the user through `Store.GetUser()`.
4. Write an HTTP 401 for missing, unknown, or revoked credentials.
5. Record repeated failures by source address.
6. Return the hash to the calling handler.

The key hash is passed into subsequent modules. No wallet address is used for
identity, and the wallet from a top-up does not change the account identity.

## 12. Balance Endpoint

`GET /v1/users/me` is the simplest authenticated workflow:

1. Authenticate the request.
2. Fetch the user from the store.
3. Serialize the `User` as JSON.

The response includes the integer balance field `balance_micro_usdc`. It does
not reveal the API-key hash.

## 13. x402 Top-Up Workflow

`internal/payment/payment.go` uses the exported x402 facilitator client
directly. It does not use the stock x402 middleware because that middleware
commits a successful response before the wrapped handler can credit Postgres.

### Initial Top-Up Request

The client sends:

```http
POST /v1/topups
Authorization: Bearer <api-key>
Content-Type: application/json

{"amount":"1.00"}
```

`httpapi.topup()`:

1. Authenticates the API key.
2. Reads the bounded request body.
3. Parses the requested amount with `money.ParseUSDC()`.
4. Rejects amounts below `MIN_TOPUP_MICRO`.
5. Asks `payment.Service.Protect()` to handle the x402 portion.

`Protect()` creates a USDC payment requirement for the configured Base network,
receiving address, exact amount, and resource URL.

If `X-PAYMENT` is absent, it returns HTTP 402 with:

```json
{
  "x402Version": 1,
  "error": "Payment required for this resource",
  "accepts": [ ... ]
}
```

The gateway does not sign on behalf of the client. The client needs an x402
aware wallet/client library to sign the USDC payment and retry.

### Verification And Settlement

On retry, `Protect()`:

1. Decodes the Base64 `X-PAYMENT` header.
2. Requires x402 version 1.
3. Finds a payment requirement matching scheme and network.
4. Calls the facilitator's `/verify` endpoint.
5. Rejects invalid verification.
6. Calls the facilitator's `/settle` endpoint.
7. Requires successful settlement, transaction hash, payer, and expected
   network.
8. Encodes the settlement as `X-PAYMENT-RESPONSE`.
9. Invokes the protected credit handler.

The facilitator is Coinbase CDP by default, but its URL and Authorization
header are configurable.

### Credit After Settlement

The protected credit handler calls:

```text
Store.CreditTopUp()
```

That transaction inserts the top-up event and increments the user's balance.
Only after that operation succeeds does `writeJSON()` send the successful
response. The local meter cache is then updated with the returned durable
balance.

The cross-system limitation is important: blockchain settlement and Postgres
cannot be atomically committed together. The explicit reconciliation command
is the recovery mechanism for an ambiguous database failure.

## 14. Ollama Adapter

`internal/ollama/client.go` is the only package that knows the Ollama HTTP
transport and stream framing. The rest of the gateway deals in generic request
bytes and the small `ollama.Event` usage view.

### Complete Proxy

`Client.Proxy()` builds a standard-library `httputil.ReverseProxy` against the
configured Ollama origin. It preserves the incoming method, path, query string,
body, ordinary headers, response status, response headers, and streaming bytes.
It removes the gateway's API-key/payment headers before forwarding and uses a
negative flush interval so streaming responses are delivered as soon as the
upstream writes them.

This is deliberately a transparent proxy instead of an implementation of every
Ollama route. Any Ollama `/api/*` or `/v1/*` endpoint, including model
management, status, blobs, OpenAI compatibility, and future routes, reaches the
same upstream path. The HTTP layer only classifies known compute paths when it
needs to add balance admission and billing.

### Request Preparation

`Client.PrepareNative()`:

1. Decodes the body as a JSON object.
2. Pins `model` to the configured model, ignoring a client-selected model.
3. Sets `stream` to the handler's selected mode.
4. Creates `options` if absent.
5. Adds or lowers `options.num_predict` so it cannot exceed the configured
   safety ceiling.
6. Marshals the normalized JSON back to bytes.

This protects the host from an arbitrary model request and from an unbounded
generation request.

`Client.PrepareModel()` pins model-only requests such as embeddings without
adding generation limits. `Client.PrepareCompatible()` preserves the
OpenAI/Anthropic-compatible body, pins `model`, and caps an existing or
endpoint-appropriate `max_*_tokens` field without removing other fields.

### HTTP Request

`Client.Do()` preserves the incoming end-to-end headers, replaces the prepared
JSON body and destination, disables redirects, and returns the upstream response
without imposing a schema. Native streaming bodies are then read from:

```text
OLLAMA_URL + "/api/generate"
OLLAMA_URL + "/api/chat"
```

The OpenAI-compatible paths use `Client.StreamSSE()` instead:

```text
OLLAMA_URL + "/v1/chat/completions"
OLLAMA_URL + "/v1/completions"
OLLAMA_URL + "/v1/responses"
OLLAMA_URL + "/v1/messages"
```

Both readers enforce a 2 MiB maximum native line/SSE-frame size. Upstream HTTP
errors and redirects are forwarded before stream parsing.

### NDJSON Events

Each native line is:

1. Copied as raw bytes.
2. Decoded into `ollama.Event`.
3. Passed to the callback.

The event captures both generation-shaped `response` text and chat-shaped
`message.content`, plus completion metadata:

- `done`.
- `prompt_eval_count`.
- `eval_count`.

The final event is required for a normal completion. EOF without a final `done`
event becomes an error.

### SSE Events

`StreamSSE()` reads complete Server-Sent Events frames, joins their `data:`
lines for observation, and forwards the original frame byte-for-byte. It recognizes
OpenAI's `data: [DONE]` terminal frame and the `response.completed` and
`message_stop` terminal event types. `EventFromJSON()` extracts only common text
and usage fields (`prompt_tokens`/`input_tokens`, `completion_tokens`/
`output_tokens`) with shallow protocol structs and merges usage split across
events. It does not recursively guess fields or translate tool calls, images,
structured output, reasoning, or other payload fields.

## 15. Inference Request Workflow

All known billable native, OpenAI-compatible, and Anthropic-compatible compute
paths call the same `inference()` function with a protocol-specific
`inferenceSpec`. Unclassified Ollama paths go directly through the transparent
proxy.

### Before Calling Ollama

The handler:

1. Authenticates the key.
2. Checks the locally available balance.
3. Rejects a non-positive available balance with HTTP 402.
4. Loads user-specific rate and concurrency limits.
5. Acquires a guardrail slot.
6. Reads the request body with the configured maximum size.
7. Parses the optional boolean `stream` field; native streaming defaults to true
   and OpenAI-compatible streaming defaults to false.
8. Calls the protocol-appropriate Ollama request preparer.
9. Creates a request timeout context.
10. Reserves the capped maximum request cost and creates a meter tracker. If the
    reservation fails, it returns HTTP 402 before calling Ollama.

Ollama is not contacted until authentication, balance, guardrail, body, and
safety checks pass.

### Streaming Mode

For native streaming mode, the handler preserves the upstream content type and
calls `Ollama.StreamNDJSON()`. OpenAI-compatible and Anthropic-compatible
streaming calls `Ollama.StreamSSE()`.

For each event, the callback:

1. Gives the event to `Tracker.Observe()`.
2. Writes the original raw native event or SSE frame to the client.
3. Flushes the writer when `http.Flusher` is available.

The HTTP status is intentionally not committed until the first output write.
This means an Ollama connection failure before any output can still become an
HTTP 502 instead of an empty HTTP 200 response.

If output has already been sent, a later failure cannot change the HTTP status.
The stream ends at that point and observed usage is finalized against the
existing reservation.

### Non-Streaming Mode

For non-streaming mode, the handler reads the upstream JSON, observes usage,
finalizes billing, and then writes the original JSON response as
`application/json`.

This mode can return a normal HTTP error if Ollama or final billing fails before
the response is written.

## 16. Metering And Reservations

`internal/meter/meter.go` separates fast local decisions from durable final
accounting.

### Balance Cache

`BalanceCache` stores, per key:

- `actual`: the latest durable balance known from Postgres.
- `reserved`: maximum cost currently held by active requests.

The available local balance is:

```text
available = actual - reserved
```

`Get()` lazily initializes a key from the Postgres value returned during
preflight. `Set()` and `AfterDebit()` adopt the authoritative durable balance
while preserving reservations belonging to active requests.

`Reserve()` atomically increases the local reservation only when enough
available balance remains. `Release()` removes a reservation when a request
never reaches final accounting.

`Meter` serializes each key's durable money operation and cache update under one
per-key lock. A top-up and a debit for the same key cannot interleave their
Postgres result and cache update, so the cache cannot be left above or below the
durable balance. Different keys never block each other.

The cache itself is protected by a mutex, so two requests cannot both reserve
the same local amount.

### Tracker

Each request gets a `Tracker` containing:

- The meter.
- Key hash.
- Start time.
- Current generated-text estimate.
- Maximum local cost reservation.
- Finalized/canceled state.

### Reservation

Creating a tracker reserves `max_tokens * PRICE_PER_TOKEN_MICRO` after gateway
policy caps the request. For embeddings, the request byte length is a
conservative input-token upper bound. Reservation failure returns HTTP 402
before Ollama receives the request.

`Tracker.Observe()` still estimates tokens from visible text, but only as a
fallback when an interrupted stream never reports exact usage. It does not make
admission or mid-stream stop decisions.

### Finalization

`Tracker.Finalize()`:

1. Uses Ollama `eval_count` when available.
2. Falls back to the local output estimate for interrupted streams.
3. Records `prompt_eval_count` for auditability.
4. Bills generated output tokens only.
5. Calls `Store.Debit()` with latency and partial state.
6. Updates the cache from the durable result and removes the full reservation.

The final cost is:

```text
completion_tokens * PRICE_PER_TOKEN_MICRO
```

Prompt tokens are recorded but not charged because the project prices the
expensive generated output rather than prompt ingestion.

`Tracker.FinalizeInput()` is used for embedding endpoints. Embeddings do not
generate completion text, so their reported input tokens are charged at the
same configured per-token rate while remaining recorded in `prompt_tokens`.
This applies to `/api/embed` and `/v1/embeddings`. Legacy `/api/embeddings` is
proxied without token billing because its response does not report usage.

## 17. Insufficient Balance Workflow

If the available balance cannot cover the capped maximum cost, inference returns
HTTP 402 before Ollama is contacted. The client can top up through `/v1/topups`
and retry. No protocol-specific depletion frame is inserted into an active
stream, so successful native and compatible streams retain Ollama's bytes.

There is no automatic top-up in the gateway. Automatic funding would require
client-side wallet authority and spending policy. An optional future MCP or
client SDK integration could implement that policy without giving the gateway
the user's private key.

## 18. Error Paths

### Authentication Errors

- Missing key: HTTP 401.
- Unknown key: HTTP 401.
- Revoked key: HTTP 401.
- Store failure: HTTP 500.

### Request Errors

- Missing body: HTTP 400.
- Body too large: HTTP 413.
- Invalid JSON: HTTP 400.
- Invalid `stream`: HTTP 400.
- Invalid normalized Ollama request: HTTP 400.

### Balance And Guardrail Errors

- Balance cannot cover the capped request reservation: HTTP 402.
- Canceled capacity wait: HTTP 429.
- Storage insufficient-funds error: HTTP 402.

### Ollama Errors

- Upstream HTTP errors preserve Ollama's status, headers, and body on both
  metered and transparent routes.
- Connection or decode error before output: HTTP 502.
- Failure after streamed output: stream termination and final billing against
  the existing reservation.
- Native EOF without `done`, or SSE EOF without a terminal event: treated as an
  upstream stream failure.

### x402 Errors

- Missing payment header: HTTP 402 with requirements.
- Malformed payment header: HTTP 400.
- Unsupported/mismatched payment: HTTP 402 with requirements.
- Facilitator verification/settlement unavailable: HTTP 503.
- Failed settlement: HTTP 402 with requirements.
- Invalid settlement metadata: HTTP 402 with requirements.

## 19. CLI Workflows

### Create An Invite

```sh
./gateway user add --config=FILE --label=demo
```

The CLI loads the database configuration, applies configured default limits,
generates a key, stores its digest, and prints the plaintext key once.

### List Users

```sh
./gateway user list --config=FILE
```

This returns user records as JSON.

### Revoke A Key

```sh
./gateway user revoke --config=FILE --key-stdin
```

The key can also come from `--key-file`. The raw `--key` flag remains available
for controlled local use but can appear in shell history or process listings.

Revocation is immediate for new requests because every request looks up the
user before inference.

### Change Limits

```sh
./gateway user limit --config=FILE --key-stdin --rate=60 --concurrency=1
```

The values are persisted in the user record and loaded on later requests.

### Reconcile A Settled Top-Up

```sh
./gateway topup reconcile --config=FILE \
  --key-stdin \
  --amount-micro=1000000 \
  --transaction=0x... \
  --payer=0x... \
  --network=base-sepolia
```

The operator must verify the transaction on-chain first. The ledger's unique
transaction hash prevents the same settlement from being credited twice.

## 20. Database Schema

`migrations/001_init.sql` creates three tables.

### `users`

This is the identity/account table. `key_hash` is unique. `balance_micro_usdc`
has a non-negative check constraint. Revocation is a boolean plus timestamp so
history remains available.

### `topups`

This is the funding audit table. `transaction_hash` is globally unique so a
settled blockchain payment cannot be credited twice.

The foreign key points to `users.key_hash`, which means a top-up cannot be
recorded for an unknown account.

### `request_ledger`

This is the usage audit table. It captures both token classes even though only
completion tokens are currently billed. It also captures latency, partial state,
and resulting balance.

There are indexes for per-key recent top-ups and request history.

## 21. Deployment Boundary

The systemd unit in `deploy/gateway.service` runs the same gateway binary with
an environment/config file. It supplies:

- Boot startup.
- Restart on failure.
- Journald logging.
- A restricted service user.
- Basic systemd filesystem/process protections.

The gateway only binds a local or host port. Cloudflare Tunnel, Tailscale
Funnel, a reverse proxy, or direct exposure is outside the binary.

A typical host layout is:

```text
/opt/payed-llm-inference/gateway
/opt/payed-llm-inference/migrations/001_init.sql
/etc/payed-llm-inference/gateway.env
```

The operator applies the SQL schema, configures Postgres/Ollama/facilitator,
creates an invite, and then starts the service. No deployment has been carried
out from this repository.

## 22. Dependency Choices

The implementation uses a small dependency surface for application code:

- Go standard library for HTTP, JSON, flags, logging, contexts, crypto, and
  synchronization.
- `github.com/jackc/pgx/v5` for Postgres.
- `github.com/mark3labs/x402-go` for x402 data structures, payment encoding,
  and facilitator HTTP calls.

No web framework is used. This keeps route behavior visible in one file and
avoids framework-specific response-writer behavior around streaming.

## 23. Tests And Verification

Current unit tests cover:

- API-key generation and request extraction in `internal/auth/auth_test.go`.
- Decimal money parsing and formatting in `internal/money/money_test.go`.
- Ollama request normalization, native usage, SSE framing, and final usage
  parsing in `internal/ollama/client_test.go`.
- Complete proxy forwarding, credential stripping, OpenAI SSE preservation, and
  management-stream passthrough in `internal/httpapi/server_test.go`.
- Per-key serialized balance-cache updates that preserve reservations in
  `internal/meter/meter_test.go`.

The latest local verification commands are:

```sh
go test ./...
go vet ./...
go test -race ./...
go build -o /tmp/payed-llm-gateway ./cmd/gateway
```

These checks cover compilation, unit tests, race detection, static analysis,
binary construction, and whitespace. They do not prove that a real Postgres
instance, Ollama server, CDP facilitator, wallet, Base Sepolia payment, tunnel,
or systemd installation works in a particular environment.

The highest-value future integration tests are:

- x402 402/verify/settle/credit sequencing with a mock facilitator.
- Duplicate and conflicting transaction hashes.
- Concurrent top-ups and debits.
- Stream flushing, truncation, disconnect, and reservation cleanup behavior.
- Final billing when Ollama sends or omits its final usage event.
- Revocation during an in-flight request.
- CLI secret input and configured defaults.
- Live Base Sepolia funding using a disposable wallet.

## 24. Known Limitations

### Interrupted Usage Estimates

Ollama final usage counts are authoritative when present. Before the final event,
the gateway estimates tokens from streamed text. This estimate is used only to
account for an interrupted stream; it is not a replacement tokenizer.

### Final Debit Durability

Only the final request debit is durable. The full local request reservation is
released at completion. A process crash may lose usage that had been generated
but not finalized; it will not create a false durable charge for unfinished output.

### Single-Process State

The cache, guardrails, and failure monitor are process-local. Running multiple
gateway replicas would require shared reservation and limit coordination that is
not implemented.

### Cross-System Settlement

Blockchain settlement and Postgres credit cannot be one transaction. The
explicit reconcile command handles the ambiguous failure case and should be
used only after on-chain confirmation.

## 25. Read-Through Summary

If reading the implementation in order, use this sequence:

1. Read `cmd/gateway/main.go` to understand process and CLI entry points.
2. Read `internal/config/config.go` to understand runtime inputs.
3. Read `internal/httpapi/server.go` to see how dependencies are composed and
   requests are orchestrated.
4. Read `internal/auth/auth.go` to understand identity lookup.
5. Read `internal/guardrails/guardrails.go` to understand request admission.
6. Read `internal/ollama/client.go` to understand upstream streaming.
7. Read `internal/meter/meter.go` to understand upfront reservations and final
   billing.
8. Read `internal/ledger/store.go` and `internal/ledger/postgres.go` to see
   durable accounting and SQL transaction boundaries.
9. Read `internal/payment/payment.go` to understand the x402 top-up handshake.
10. Read `migrations/001_init.sql` to connect the SQL tables to the Go methods.
11. Read `README.md` for the operator sequence.
12. Read `engineering-choices.md` for deliberate deviations and limitations.

The most important mental model is that the API key is the account, Postgres is
the durable money ledger, the local meter reservation is only a concurrency
optimization, Ollama is an already-running backend, and x402 is used only to
fund the account.
