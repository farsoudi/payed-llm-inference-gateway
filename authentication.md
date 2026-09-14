# Client Authentication
*Auto-Generated with GPT-5.6 Luna*

This document explains how a client on another machine authenticates to the
gateway, how to keep the API key in the current shell or future shell sessions,
and how authentication fits into top-ups and inference.

The gateway is intended to be reached through a tunnel, reverse proxy, or
direct HTTPS exposure. The client talks to the gateway URL, not directly to
Ollama.

```text
Client -> public gateway URL -> Go gateway -> private Ollama
```

## 1. What The API Key Represents

An API key is the account identity in this system. It owns:

- The prepaid balance.
- Top-up credits.
- Inference debits.
- Rate limits.
- Concurrency limits.
- Balance and usage history.
- Revocation state.

The wallet used to pay USDC is not the identity. A client can use different
wallets for different top-ups and still fund the same API-key account.

The API key is required for all authenticated gateway routes:

- `GET /v1/users/me`
- `POST /v1/topups`
- `POST /v1/generate`
- `POST /v1/chat/completions`
- Every other Ollama `/api/*` and `/v1/*` route

`GET /healthz` is the only unauthenticated route.

## 2. Receiving A Key

An administrator creates an invite with:

```sh
./gateway user add --config=/path/to/gateway.env --label=demo
```

The command generates a random key beginning with `pli_`, stores only its
SHA-256 digest, and prints the plaintext key once. The database never contains
the recoverable plaintext key.

The person or process receiving the key must store it securely. If the key is
lost, it cannot be recovered from the gateway; an administrator must revoke it
and issue a new one.

## 3. Supported Authentication Headers

### Preferred: Bearer Authorization

Send the key as a Bearer token:

```http
Authorization: Bearer pli_your_api_key
```

This is the preferred form because standard API clients and OpenAI-style
provider libraries commonly generate it automatically from an API-key setting.

### Alternative: `X-API-Key`

The gateway also accepts:

```http
X-API-Key: pli_your_api_key
```

This is useful for simple scripts or clients that support custom headers but do
not provide normal Bearer authentication.

Do not send both headers unless there is a specific reason. Do not place the key
in a URL, query string, request body, or log message.

## 4. Current Shell Session

For a temporary session, export the gateway URL and key:

```sh
export PAYED_GATEWAY_URL='https://gateway.example.com'
export PAYED_GATEWAY_API_KEY='pli_replace_with_real_key'
```

The variables exist only in the current shell and programs launched from it.
They disappear when that shell exits.

Use them with `curl` like this:

```sh
curl "$PAYED_GATEWAY_URL/v1/users/me" \
  -H "Authorization: Bearer $PAYED_GATEWAY_API_KEY"
```

The variable names are client-side conventions. The gateway does not require
these exact names; it only receives the resulting HTTP header.

## 5. Persisting In A Shell RC File

To make the variables available in future interactive Bash sessions, add them
to `~/.bashrc`:

```sh
export PAYED_GATEWAY_URL='https://gateway.example.com'
export PAYED_GATEWAY_API_KEY='pli_replace_with_real_key'
```

For Zsh, use `~/.zshrc` instead:

```sh
export PAYED_GATEWAY_URL='https://gateway.example.com'
export PAYED_GATEWAY_API_KEY='pli_replace_with_real_key'
```

Reload the file in the current shell:

```sh
source ~/.bashrc
```

or:

```sh
source ~/.zshrc
```

Protect the file because it contains a credential:

```sh
chmod 600 ~/.bashrc
chmod 600 ~/.zshrc
```

Do not commit either file to a repository or paste its contents into issue
trackers. Shell environment variables are inherited by child processes, so
only run commands you trust from a shell containing the key.

## 6. Separate Client Environment File

A separate file avoids putting the key directly in a large shell configuration
file:

```sh
mkdir -p ~/.config/payed-llm-inference
chmod 700 ~/.config/payed-llm-inference
```

Create `~/.config/payed-llm-inference/client.env` with shell-safe contents:

```sh
PAYED_GATEWAY_URL='https://gateway.example.com'
PAYED_GATEWAY_API_KEY='pli_replace_with_real_key'
```

Protect it:

```sh
chmod 600 ~/.config/payed-llm-inference/client.env
```

Load it for the current shell:

```sh
set -a
source ~/.config/payed-llm-inference/client.env
set +a
```

`set -a` exports variables created while the file is sourced. Only source a
file you created or trust; sourcing an arbitrary file executes shell code.

To load it automatically for interactive Bash sessions, add this line to
`~/.bashrc`:

```sh
[ -f "$HOME/.config/payed-llm-inference/client.env" ] && source "$HOME/.config/payed-llm-inference/client.env"
```

Use the equivalent line in `~/.zshrc` for Zsh.

## 7. Checking Authentication

The balance endpoint is the simplest authentication test:

```sh
curl -i "$PAYED_GATEWAY_URL/v1/users/me" \
  -H "Authorization: Bearer $PAYED_GATEWAY_API_KEY"
```

Expected outcomes:

- `200 OK`: key is valid and the account record was read.
- `401 Unauthorized`: key is missing, unknown, or revoked.
- `404 Not Found`: generally indicates an unrelated route/path problem, not a
  valid authenticated account response.
- `500 Internal Server Error`: gateway could not complete the ledger lookup.

The response contains the account balance in micro-USDC and does not expose the
raw API key.

## 8. Authentication And Top-Ups

The API key alone does not make a top-up. It identifies which account receives
the credit. The client also needs an x402-capable wallet/payment client.

The complete request has two credentials/identities:

```text
API key       -> identifies the gateway account
X-PAYMENT     -> carries the client's signed USDC payment
```

### Step 1: Ask For A Top-Up

```sh
curl -i "$PAYED_GATEWAY_URL/v1/topups" \
  -H "Authorization: Bearer $PAYED_GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"amount":"1.00"}'
```

The gateway validates the amount and returns HTTP `402 Payment Required` with
the exact Base USDC payment requirement.

Plain `curl` stops at this point. It does not have a wallet signer and cannot
complete the payment.

### Step 2: Pay With An x402-Aware Client

An x402-aware client reads the 402 requirements, signs the USDC payment using a
client-controlled wallet, and retries the same request with:

```http
X-PAYMENT: <base64-encoded-x402-payment-payload>
```

The wallet must have the correct network's funds. For development, use Base
Sepolia test ETH and test USDC. Never use a production private key for a first
integration test.

### Step 3: Gateway Verification

The gateway:

1. Decodes the x402 payment payload.
2. Confirms it matches the requested amount and Base network.
3. Calls the configured facilitator's verification endpoint.
4. Calls the facilitator's settlement endpoint.
5. Requires a successful transaction, payer, and expected network.
6. Inserts the top-up event and increments the API-key balance in Postgres.
7. Returns `X-PAYMENT-RESPONSE` with settlement information.

The key is used on both the initial 402 request and the paid retry. A payment
without the API key cannot select an account to credit.

## 9. Making Inference Requests

Once the account has enough balance to reserve the request's capped output, the
client continues to use the same API key.

```sh
curl -N "$PAYED_GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $PAYED_GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "messages":[
      {"role":"user","content":"Explain x402 briefly"}
    ],
    "stream":true
  }'
```

The gateway authenticates the key, checks its balance and limits, calls the
configured Ollama model, streams the response, and debits generated output
usage. The native route keeps Ollama's NDJSON format:

```sh
curl -N "$PAYED_GATEWAY_URL/api/generate" \
  -H "Authorization: Bearer $PAYED_GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Explain x402 briefly","stream":true}'
```

The OpenAI-compatible route keeps SSE instead:

```sh
curl -N "$PAYED_GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $PAYED_GATEWAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"ignored-by-gateway","messages":[{"role":"user","content":"Explain x402 briefly"}],"stream":true}'
```

Native `/api/*` requests retain Ollama JSON/NDJSON. OpenAI-compatible `/v1/*`
requests retain Ollama's OpenAI JSON/SSE surface, including tool-call fields.
Unmetered routes are transparently proxied. Metered routes are
protocol-compatible: the gateway pins policy fields, observes documented
usage fields, and reserves the capped request cost before calling Ollama.

## 10. Remote OpenCode Authentication

OpenCode can generally be configured with a custom provider URL and an API key
environment variable. The authentication portion is straightforward:

```json
{
  "provider": {
    "payed-gateway": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Paid LLM Gateway",
      "options": {
        "baseURL": "{env:PAYED_GATEWAY_URL}/v1",
        "apiKey": "{env:PAYED_GATEWAY_API_KEY}"
      }
    }
  }
}
```

This should cause the client provider to send:

```http
Authorization: Bearer <PAYED_GATEWAY_API_KEY>
```

That header is accepted by the gateway, and the `/v1` request/response protocol
is forwarded as OpenAI-compatible JSON/SSE. The gateway still pins the model and
caps output tokens according to its configuration.

OpenCode also cannot automatically perform the gateway's x402 top-up merely
because an API key is configured. The account must be funded separately by an
x402-aware wallet client, or a future client-side spending integration must be
used.

## 11. Tool Calls And Authentication

The API key authenticates the request regardless of whether the body contains
normal messages, images, structured output, or tool definitions.

The gateway should remain a transport, accounting, and policy boundary. It
should not execute tools or MCP operations and should not receive the client's
private wallet key.

The wildcard proxy forwards unknown JSON fields and tool definitions to Ollama
without executing them. In a complete client loop:

```text
OpenCode sends tool request
  -> gateway authenticates and proxies
  -> Ollama emits tool call
  -> OpenCode executes the tool
  -> OpenCode sends tool result through gateway
```

## 12. Key Rotation And Revocation

API keys are credentials, not passwords that can be displayed again. If one may
have leaked:

1. Stop using the compromised key.
2. Revoke it:
   ```sh
   ./gateway user revoke --config=FILE --key-stdin
   ```
3. Issue a replacement:
   ```sh
   ./gateway user add --config=FILE --label=replacement
   ```
4. Update the client's current shell, rc file, secret manager, or provider
   configuration.

Revocation blocks new top-ups, balance checks, and inference requests. Existing
in-flight inference is not cryptographically canceled by revocation; the
single-process request still has to reach its normal completion or error path.

## 13. Secrets And Threat Model

### Do

- Use HTTPS for a tunneled or externally reachable gateway.
- Keep the API key in a protected file or secret manager.
- Use `--key-file` or `--key-stdin` for administrative operations.
- Use a separate disposable wallet for Base Sepolia testing.
- Set spending limits in any client that implements automatic top-ups.
- Rotate and revoke keys when access changes.

### Do Not

- Put an API key in a URL.
- Commit an API key, shell rc file, or client env file.
- Put a wallet private key in the gateway configuration.
- Use a real Base wallet while testing Base Sepolia.
- Assume an x402 payment alone identifies the gateway account.
- Assume the gateway will automatically fund an account with insufficient balance.

The gateway logs request metadata such as method, path, status, and duration.
It does not intentionally log the raw API key, prompt, completion, tool
arguments, or wallet private key.

## 14. Troubleshooting

### HTTP 401

Check that the variable is set in the shell running the client:

```sh
printf '%s\n' "${PAYED_GATEWAY_API_KEY:+API key is set}"
```

Do not print the key itself. Check that the header is formed as:

```text
Authorization: Bearer <key>
```

Also check that the key has not been revoked or accidentally truncated by shell
quoting.

### HTTP 402 On Inference

This means the key is valid but its available balance is not positive or is
insufficient for the request. Top up the account separately. Inference does not
accept an x402 payment directly in the current prepaid design.

### HTTP 402 On Top-Up

This is expected for the first top-up request. A payment-aware client must read
the requirements, sign the payment, and retry with `X-PAYMENT`.

### HTTP 503 On Top-Up

The gateway could not reach or use the facilitator for verification or
settlement. Check facilitator URL, authorization, network selection, and
outbound connectivity. Do not repeatedly submit real payments while debugging.

### OpenCode Auth Works But Requests Fail

This usually means the Bearer header is correct but the selected model/provider
configuration is not. The gateway preserves native Ollama JSON/NDJSON under
`/api/*` and OpenAI-compatible JSON/SSE under `/v1/*`; authentication and
provider configuration remain separate concerns.

## 15. Client Checklist

Before using a client from another machine:

- Gateway has a reachable HTTPS/tunnel URL.
- Client has the correct API key.
- API key is exported in the current shell or securely loaded from an rc/env
  file.
- `GET /v1/users/me` returns 200.
- Account has a balance from a completed x402 top-up.
- Client sends `Authorization: Bearer <key>` or `X-API-Key`.
- Client uses native Ollama JSON/NDJSON or OpenAI-compatible JSON/SSE according
  to the selected route.
- Client handles HTTP 402 when its balance cannot cover the requested output
  limit.
- Client does not expect the gateway to execute tools.
- Client does not send a wallet private key to the gateway.

## 16. Files To Read Next

- `README.md`: setup and command examples.
- `model.md`: intended payment, reservation, and settlement behavior.
- `code-architecture.md`: source-level module and workflow explanation.
- `engineering-choices.md`: deliberate implementation choices and limitations.
- `internal/auth/auth.go`: exact header extraction and hashing behavior.
- `internal/httpapi/server.go`: where authentication is applied to routes.
- `internal/payment/payment.go`: x402 top-up authentication/payment sequence.
