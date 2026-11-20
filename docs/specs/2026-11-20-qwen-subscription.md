# Qwen subscription support in NEXUS

## Goal

Let an operator connect a Qwen account with a Code subscription to NEXUS and
use the models Qwen reports for that session through the existing proxy APIs.
Clients select the upstream's model ID, and NEXUS keeps routing that model to
the last provider that served it successfully while that provider is eligible.

Qwen subscription access tokens expire and refresh can rotate the refresh
token. A static bearer token therefore requires ongoing manual replacement.
The Pi extension shows that Qwen device sign-in, model discovery, and automatic
token rotation can work together. NEXUS needs the same capabilities for its own
deployment. [Qwen subscription protocol](https://github.com/QwenLM/qwen-code/blob/main/docs/design/qwen-subscription.md#protocol-and-credential-security)

## Scope

Support one NEXUS-managed Qwen subscription session per deployment, with
native dashboard and terminal sign-in. The dashboard is the primary setup
experience. The proxy supports the subscription for both `POST /v1/chat/completions`
and `POST /v1/messages`, using its OpenAI-compatible upstream support. The
credential interface must be reusable by future providers with renewable bearer
tokens. NEXUS creates and owns its session independently of Pi and Qwen Code.

## Sign-in and configuration

- **Dashboard.** Show **Connect Qwen** on the NEXUS dashboard (default port
  `2222`), including on deployments that already have providers. After the
  operator starts sign-in, provide the Qwen authorization link and any user code
  needed to approve it in a separate browser tab. The dashboard shows approval
  pending, successful sign-in, or a clear next action if the flow expires, is
  denied, or fails. The operator can return to the dashboard to see the result.
- **Terminal.** A native NEXUS command starts the same device authorization
  flow, prints the link and any user code, and reports the result. It works with
  a browser on another machine, including when NEXUS runs in Docker on `rpi`.
- **Shared session.** Both entry points save to the same session in the NEXUS
  deployment. NEXUS exchanges the approval with Qwen and checks subscription
  access. The browser only needs the approval link, user code, and sign-in
  status. Cancelling, expiring, or superseding an attempt leaves the existing
  session intact. A stale attempt cannot replace newer credentials.
- **Explicit setup.** The provider is enabled by operator action. Dashboard
  and terminal setup can create the provider entry. Manual setup uses
  `type = "qwen-subscription"` under `[[providers]]` in `config.toml` and
  either sign-in path. The entry selects Qwen's subscription gateway. OAuth
  secrets live in private NEXUS state, separate from the provider config.
- **Activation.** A first-time configuration change may use the existing
  startup-based provider loading. If a restart is required, say so and
  distinguish saved sign-in from routing readiness. A running proxy with Qwen
  already enabled adopts a successful re-login automatically. A successful
  sign-in and an unavailable model listing are separate states.
- **Access.** Limit session management to the deployment operator and scope
  browser sign-in controls and pending attempt details to the dashboard. Keep
  OAuth credentials on the server.

## Refresh and request behavior

- Model discovery, chat, and recovery probes use the current session. Refresh
  automatically when the access token is close to expiry. Persist the complete
  replacement token pair atomically in private, writable state that survives
  process and container restarts. Sign-in and refresh run in NEXUS without a
  Qwen Code installation in the service container.
- Concurrent requests using the same expired or rejected token cause a single
  refresh. A later caller uses the token already saved by that refresh. An
  upstream `401` can trigger one retry with a current token on the same provider,
  before any response content has been sent to the client. Provider identity
  and quota cooldown follow the subscription account across token rotations.
- Distinguish a revoked or expired refresh grant from a transient error, a
  refresh rate limit, a membership failure, and inference quota or rate limits.
  A revoked or expired refresh grant requires explicit re-login. Only expiry or
  token rejection triggers refresh. Honor Qwen's refresh `Retry-After` on
  subsequent refresh attempts. Inference quota and rate limits follow NEXUS's
  existing provider failover behavior.
- Send tokens only to the intended Qwen endpoints. Keep token values out of
  output, logs, and diagnostic errors. After a failed auth recovery, use the
  next eligible provider when available. If the requested model cannot be
  served and Qwen needs a new sign-in, report the re-login action.

## Model discovery and routing

- Learn the subscription's model IDs from the authenticated Qwen gateway and
  publish them through NEXUS's existing `/v1/models` catalog. Clients use the
  exact upstream IDs without a provider prefix. A request for a discovered ID
  reaches Qwen with that ID by default.
- Keep the last successful catalog for the same session during transient
  discovery failures. New sign-ins establish their own catalog. Catalog
  requests from clients read NEXUS's cached snapshot. The background discovery
  process performs upstream model listing.
- For a known model ID, routing and failover consider only eligible providers
  that report that ID. On success, prefer that provider for the same model on
  subsequent requests, including after a token rotation. A deployment can
  use Qwen alongside its existing API-key providers. [Model candidates](../../internal/router/router.go#L250) · [Sticky routing](../../internal/proxy/handler.go#L572)

## Acceptance criteria

1. Starting with a deployment that has no Qwen session, complete dashboard
   sign-in using a browser on another machine. The dashboard reports sign-in
   status and model readiness. The terminal can independently authorize or
   reauthorize the same deployment and the dashboard reflects the result.
2. A discovered Qwen ID appears verbatim in `/v1/models`. Chat succeeds
   through both proxy APIs with that ID. A streaming response is relayed once.
   When two providers report the ID, repeat requests prefer the last successful
   provider and can fall back to the other on a retryable failure.
3. Concurrent discovery and chat with an expired token cause exactly one
   refresh. A stale `401` adopts an already-rotated token, and a repeated `401`
   stops after the one retry. A recreated container can use the saved session
   and refresh it again from persistent state.
4. A failed or expired sign-in leaves an existing session usable. A revoked
   refresh grant produces re-login guidance when no eligible peer can serve the
   request. Inference quota exhaustion follows the existing failover path
   without triggering OAuth refresh. Transient discovery failure retains the
   catalog for the current session. Requests from unrelated origins are denied
   access to dashboard sign-in actions and pending attempt details.
5. Verify sign-in, rotation, routing, and failure cases against controlled
   OAuth and gateway responses. Verify the real Qwen authorization and gateway
   contract with a minimal Docker smoke test using an authorized session. The
   operator documentation covers dashboard and terminal setup, manual TOML
   configuration, persistent credential storage, and re-login.
