# Plan: Image support on the OpenAI gateway (all provider paths)

Branch: `fix/gateway-image-support` (git worktree, branched from `main` at 643450b)
Worktree (all code work happens HERE, never in the main checkout):
`/private/tmp/claude-501/-Users-av-sources-github-com-nexus-proxy/67a2ba1f-ffc7-42e0-834f-b98a6aa9429e/scratchpad/nexus-imgfix`
Main checkout `/Users/av/sources/github.com/nexus-proxy` has another session's uncommitted
work (`internal/proxy/usage.go`, `internal/proxy/usage_events_test.go`, `web/src/App.svelte`)
and that session keeps committing to `main`. Do not touch the main checkout.

## Background (verified by reading code and by a live request)

A real request from pi's vision agent shape (`POST /v1/chat/completions`, `content` array with an
`image_url` part holding a base64 data URI, model `kimi-k3`) against the deployed NEXUS returns
`400 invalid request body: content: unsupported content part type "image_url" (only "text" is supported)`.

Root cause: commit d45c368 added image conversion only for the Anthropic-format inbound path
(`/v1/messages` -> OpenAI-compatible providers). The OpenAI-format inbound path
(`/v1/chat/completions`, `internal/proxy/gateway.go`) parses the body into `OpenAIRequest` first, and
`OpenAIContent.UnmarshalJSON` (`internal/proxy/transformer.go`) rejects every non-text part.

Reading the surrounding code showed the 400 is only the first of several image defects on the
gateway and adjacent paths. Once the parser is fixed, each of these would surface:

1. `OpenAIContent.Anthropic()` turns EVERY part into `{"type":"text","text":p.Text}`. An image part
   becomes an empty text block, which Anthropic-format providers (anthropic, bedrock, vertex) reject.
2. `hasImageBlock` (`semantic.go`) only matches `type == "image"` (Anthropic shape). An OpenAI
   `image_url` request would not skip the semantic cache, so same-text/different-image requests could
   be served each other's cached answer (the exact bug d45c368 fixed for the Anthropic shape).
3. The privacy firewall (`firewall.go`) regex-scans every JSON string, including base64 image
   payloads. Random base64 can contain `AIza` + 20 URL-safe chars (the Gemini key detector); that
   silently rewrites the image bytes into a placeholder and corrupts the image sent upstream.
4. The vision_model override and `nexusImages` flag exist only in `callUpstreamOnce`. The gateway's
   raw passthrough (`callOpenAIPassthrough`) never applies it, and request/usage logs record the
   plain `MapModel` result as `model_used` even when the vision model was used.
5. `hasImageContent` (`handler.go`) is a shallow scan and ignores images nested in `tool_result`
   content. That is exactly how Claude Code delivers an image read from disk, so the vision_model
   override is silently skipped for the most common Claude Code image flow.
6. Inspector capture (`--inspect`) stores `json.Marshal(req)` capped at 64KB, i.e. truncated base64
   noise per image request.

Decisions (made here, with reasons):
- No NEXUS-side fetching of http(s) image URLs and no built-in vision-model catalog. CLAUDE.md
  records why a catalog was rejected; URL fetching would add SSRF/size/timeout surface to a
  forwarder. URLs are forwarded as received; providers that cannot fetch URLs return their own
  error, which is relayed unchanged (same philosophy as the existing "no chain filtering").
- Inbound `image_url` is validated strictly at parse time to the shapes the OpenAI spec defines:
  `http(s)://...` URL or `data:<mime>;base64,<payload>`. This guarantees later conversion to
  Anthropic blocks can never silently drop an image. Other part types (`input_audio`, `file`, ...)
  stay rejected, with a clearer message.
- The shallow-scan decision documented in CLAUDE.md for `hasImageContent` is deliberately reversed
  (item I2) because it defeats the vision override for Claude Code's tool_result images. CLAUDE.md is
  updated in I7.
- `StrategyDirect` semantics are unchanged: requested model verbatim, no vision override, images
  included (CLAUDE.md "Direct Strategy"). The deployed instance uses `direct`.

Out of scope (observed, not touched): the OpenAI->Anthropic gateway conversion does not translate
`tool_calls` / `role:"tool"` messages, and `convertMessage` appears to drop `tool_result` blocks whose
`content` is a plain string. These are pre-existing, unrelated to images, and not part of this plan.

## Process

1. Work through items in the execution order below, one at a time.
2. **Delegate each item to a sub-agent. This is mandatory, not advisory.** The user's instruction
   to execute this plan is the authorization to spawn sub-agents. Do not re-ask per item. Do not
   execute items inline.
   - Tool: `Agent` with `subagent_type: "general-purpose"`.
   - Delegate by pointer: absolute path of this plan file, the item ID, the line range of the item's
     detail section with its exact heading as anchor, the status vocabulary, the stop conditions.
     Map the document once with `rg -n "^### " <plan file>`.
   - The sub-agent reads only its own section with `Read` offset/limit, and verifies the section
     starts with the heading it was given (otherwise re-locate with `rg -n "^### <ID>"`).
   - Sub-agents run in the background; the main agent does not poll or predict results, and relays
     what changed after each item (the sub-agent's report is not shown to the user).
   - Sub-agents never question the user; decisions route through the main agent.
   - If a spawn fails, stop and report. Never fall back to inline execution silently.
3. The sub-agent does the pre-work assessment. Multiple reasonable approaches with real trade-offs
   -> stop, report `blocked_needs_user_input` with the options. A blocker -> stop, report `blocked`.
   Unambiguous path -> proceed.
4. The sub-agent never edits this document. It reports exactly one status: `blocked_needs_user_input`,
   `blocked`, `done`, or `skipped` (with reason). The main agent is the sole writer of this file.
5. The sub-agent reads the relevant current code before modifying it.
6. Verification for Go work: `gofmt -l <changed files>` prints nothing, `go vet ./internal/...`,
   `go build ./...`, and `go test ./internal/proxy/ ./internal/providers/ ./internal/router/` (plus
   any package touched), run from the worktree directory. Never run `go` commands in the main
   checkout (it contains another session's uncommitted edits).
7. Commits: each code item ends with ONE commit on branch `fix/gateway-image-support`, made from the
   worktree. Stage by explicit path (`git add <file> <file>`), NEVER `git add -A` / `git add .` /
   `git commit -a` (this plan file is untracked and must not be swept in). Build and tests must pass
   before committing. Commit message style: conventional (`fix(proxy): ...`, `test(proxy): ...`,
   `docs: ...`), a short subject, a wrapped body explaining why, and this exact trailer as the last line:
   `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`
   Do NOT push. Pushing and deploying are separate items run only on the main agent's instruction.
8. The sub-agent ends its report with a one-to-two-line completion note: what changed, what was
   verified, the commit hash.
9. Do not batch unrelated items. Grouped items may share one sub-agent.
10. Never use `sed` or `awk` for editing or searching; use the Edit/Read tools and `rg`.
    The remote shell on the Pi is fish: wrap remote scripts as `ssh rpi bash -s <<'EOF' ... EOF`.

## Dependency Analysis

- I1 (parser + Anthropic conversion) is the foundation: every gateway-level test of the later items
  needs the request to parse. I1 first.
- I2 (shared image detector) is needed by I4 (the gateway computes `nexusImages` with it) and by I6.
  It replaces `hasImageBlock` and `hasImageContent`. I2 before I4.
- I3 (firewall) shares no code with the others (only `firewall.go`), but its E2E coverage in I6 needs
  I1. Independent otherwise.
- I4 and I5 both edit `logResult` in `handler.go`; run sequentially (I4 then I5) in the one worktree.
- I6 (cross-path end-to-end test matrix) needs I1-I5.
- I7 (docs) needs final behavior from I1-I6.
- V1 (full verification in a clean checkout of the committed state) needs all code items.
- D1 (merge to `main`, push to `opi`) needs V1 and a user decision on deploy scope (see D1).
- D2 (deploy) needs D1. D3 (live end-to-end) needs D2.
- Shared files: `transformer.go` (I1), `semantic.go`+`handler.go`+new `images.go` (I2), `firewall.go`
  (I3), `gateway.go`+`handler.go`+`openai.go` (I4), `handler.go` (I5). `gateway.go`, `handler.go`,
  `openai.go` were also edited by the other session's recent commits; the worktree is based on its
  latest commit so those edits are already present.

## Execution Order

| # | ID | Title | Severity | Status | Group | Backend | Verification |
|---|----|-------|----------|--------|-------|---------|--------------|
| 1 | I1 | Gateway accepts image_url parts; Anthropic-format conversion | Critical | done | -- | No | unit + gateway tests, build, vet |
| 2 | I2 | One recursive image detector (semantic cache + vision flag) | High | done | -- | No | unit tests incl. tool_result + image_url |
| 3 | I3 | Firewall never scans/rewrites base64 image payloads | High | done | -- | No | firewall tests with AIza collision |
| 4 | I4 | Vision override + model attribution on gateway and logs | High | done | Grouped: Model/Log | No | gateway + log tests |
| 5 | I5 | Inspector capture omits base64 image payloads | Low | done | Grouped: Model/Log | No | inspect test |
| 6 | I6 | End-to-end image test matrix across all gateway paths | High | done | -- | No | new test file passes with -race |
| 7 | I7 | Docs: CLAUDE.md, CHANGELOG, provider notes | Medium | done | -- | No | rg checks, no stale "shallow" claims |
| 8 | V1 | Full verification of committed branch in a clean state | High | done | -- | No | gofmt, vet, build, test -race, diff scope |
| 9 | D1 | Merge to main, push to opi (needs user decision) | High | pending | -- | No | git log, remote ref |
| 10 | D2 | Deploy to the Raspberry Pi (docker compose) | High | pending | -- | No | health, version, logs |
| 11 | D3 | Live end-to-end image test through the real proxy | High | pending | -- | Needs Pi up | real image described |

## Item Details

### I1 Gateway accepts image_url parts; Anthropic-format conversion

- **What:** Make `POST /v1/chat/completions` accept OpenAI `image_url` content parts and carry them
  correctly to every provider type.
- **Why:** This is the reported bug. `OpenAIContent.UnmarshalJSON` rejects any non-text part with a 400;
  and `OpenAIContent.Anthropic()` would turn image parts into empty text blocks for Anthropic-format
  providers (anthropic, bedrock, vertex), which they reject. See Background items 1 and the root cause.
- **How:** Work in the worktree `/private/tmp/claude-501/-Users-av-sources-github-com-nexus-proxy/67a2ba1f-ffc7-42e0-834f-b98a6aa9429e/scratchpad/nexus-imgfix`.
  Read `internal/proxy/transformer.go` (types `OpenAIContent`, `OpenAIContentPart`, `OpenAIImageURL`,
  `UnmarshalJSON`, `MarshalJSON`, `Anthropic`), `internal/proxy/gateway.go`
  (`HandleChatCompletions`, `TransformOpenAIToAnthropic`) and `internal/proxy/transformer_test.go`.
  Then:
  1. Add `Detail string \`json:"detail,omitempty"\`` to `OpenAIImageURL` so the OpenAI `detail` field
     round-trips.
  2. In `UnmarshalJSON`, accept parts of type `text` (unchanged) and `image_url`. For `image_url`
     require a non-nil `image_url` object with a non-empty `url` that is either an `http://` or
     `https://` URL, or a `data:<mime>;base64,<non-empty payload>` data URI. Anything else returns an
     error that names the problem (e.g. `content: image_url part has an invalid url (want an http(s)
     URL or a base64 data URI)`). Every other part type returns an error that lists the supported
     types: `content: unsupported content part type %q (supported: "text", "image_url")`. Keep `c.text`
     as the concatenation of TEXT parts only and keep `c.parts` as today.
  3. `Anthropic()`: keep the text-block output for text parts; convert each `image_url` part, in order,
     to an Anthropic image block: data URI -> `{"type":"image","source":{"type":"base64",
     "media_type":<mime>,"data":<payload>}}`; http(s) URL -> `{"type":"image","source":{"type":"url",
     "url":<url>}}`. Parse the data URI with simple string splitting (no regex needed). Because
     UnmarshalJSON already validated, conversion cannot fail; do not silently drop anything.
  4. `MarshalJSON`: currently emits the array only when `asArray` is set. Also emit the parts array when
     `parts` contains any `image_url` part, so an unmarshaled-then-marshaled content can never silently
     lose its images. Text-only content must keep marshaling exactly as today (flat string unless
     `asArray`).
  5. Update the existing test case `unsupported part type is rejected` (it currently uses `image_url`);
     use `input_audio` instead and assert the message mentions the supported types.
  Do NOT change `gateway.go` routing logic in this item; `callOpenAIPassthrough` already forwards the
  raw body verbatim, which is correct for OpenAI-compatible providers.
- **Backend needed:** No.
- **Verify:** New unit tests in `transformer_test.go`: data URI accepted; https URL accepted; `detail`
  preserved; `ftp://` and `data:text/plain,abc` (non-base64) rejected; empty url and missing
  `image_url` object rejected; `input_audio` rejected with supported-types message; `Anthropic()` of
  a text+image(data)+image(url) content yields blocks in order with correct sources; marshal of
  unmarshaled image content keeps the images; text-only content marshal unchanged. New gateway
  tests in `gateway_test.go`: (a) an `image_url` request to an OpenAI-compatible mock provider returns
  200 and the mock receives the `image_url` part byte-identical (data URI and `detail`), with the model
  swapped as before; (b) the same request routed to an Anthropic-format provider mock (look at how
  `buildTestHandler`/`testProv`/`providers.New` build one with a custom base URL in the existing tests)
  receives a `/v1/messages` body containing a correct base64 image block and no empty text blocks.
  Then the standard verification (Process item 6). Commit: `fix(proxy): accept image_url content
  parts on the OpenAI gateway` (body: root cause = parser rejected non-text parts; Anthropic()
  conversion; strict validation rationale).

### I2 One recursive image detector (semantic cache + vision flag)

- **What:** Replace the two divergent, shallow image scans (`hasImageBlock` in `semantic.go`,
  `hasImageContent` in `handler.go`) with one shared recursive detector that understands both the
  Anthropic and the OpenAI shapes.
- **Why:** `hasImageBlock` only matches `type == "image"`, so an OpenAI `image_url` request would not
  skip the semantic cache (same-text/different-image collision). `hasImageContent` ignores images
  nested in `tool_result` content, so the vision_model override is skipped for the way Claude Code
  delivers images read from disk. Background items 2 and 5. NOTE: CLAUDE.md currently documents
  `hasImageContent` as "shallow by design"; this item intentionally reverses that (docs in I7).
- **How:** Work in the worktree (see I1 for the absolute path). Read `internal/proxy/semantic.go`
  (`promptText`, `hasImageBlock`, `collectText`), `internal/proxy/handler.go` (`hasImageContent` and its
  call in `HandleMessages`), and the existing tests `TestSemanticCacheSkipsImageRequests` (semantic_test.go),
  and the `hasImageContent`/image tests around handler_test.go line 391+. Then:
  1. Create `internal/proxy/images.go` with `contentHasImage(v interface{}) bool` (v is a message
     `content`, a `system` value, or a block slice): true if any block has `type` in
     {`image`, `image_url`, `input_image`}; for any block that has a `content` field that is itself an
     array (tool_result), recurse into it, with a small depth cap (e.g. 8) so hostile nesting cannot
     recurse unboundedly. Also add `messagesHaveImage(msgs []map[string]interface{}) bool` that applies
     it to each message's `content`.
  2. `promptText` uses `contentHasImage` for `system` and each message `content` (drop `hasImageBlock`).
  3. `HandleMessages` sets `req.nexusImages = messagesHaveImage(raw.Messages)`; delete
     `hasImageContent`. Fix every reference, including tests that call `hasImageContent`/`hasImageBlock`
     (grep tests; keep their intent, point them at the new function).
  Keep the router untouched: images still never affect `RouteChain`/classification.
- **Backend needed:** No.
- **Verify:** Table test for `contentHasImage` (string content false; text-only false; `image` true;
  `image_url` true; `input_image` true; image nested in tool_result content true; deeply nested beyond
  the cap does not panic). Tests: `promptText` reports `hasImages` for the OpenAI shape and for a
  tool_result-nested image; semantic-cache test through `HandleChatCompletions` with two `image_url`
  requests (same text, different data URI) -> two upstream calls, neither a semantic HIT (the existing
  `TestSemanticCacheSkipsImageRequests` is the template, `semanticMock` the upstream); a `HandleMessages`
  test where the only image sits inside a `tool_result` and the provider has a `vision_model` ->
  upstream gets the vision model. Existing image tests still pass. Standard verification. Commit:
  `fix(proxy): detect image_url and tool_result images with one shared scan`.

### I3 Firewall never scans or rewrites base64 image payloads

- **What:** The privacy firewall must leave base64 image payloads byte-for-byte untouched.
- **Why:** `redact` walks every JSON string value with regexes. Standard base64 can contain `AIza`
  followed by 20+ `[A-Za-z0-9_-]` characters (the Gemini key detector) or `AKIA`+16 uppercase/digits;
  a match replaces part of the image data with a placeholder and corrupts the image. A 1 MB image has
  roughly a few percent chance of hitting `AIza`. Background item 3. Placeholders are only restored in
  responses, never in the upstream request.
- **How:** Work in the worktree (see I1). Read `internal/proxy/firewall.go` (`redact`, `redactString`,
  `walk`) and `internal/proxy/firewall_test.go`. In the `walk` closure: (a) in the `map[string]interface{}`
  case, skip (do not walk) the `data` value when the same map's `type` is `"base64"` (Anthropic image
  `source`); (b) in the `string` case, return the string untouched when it is a base64 data URI
  (starts with `data:` and contains `;base64,` within its first ~128 bytes) - covers OpenAI `image_url.url`.
  Do NOT skip http(s) image URLs (they can carry credentials in query strings; existing scanning
  semantics stay). Do not change detectors or placeholders. Keep the function allocation-light:
  the check is a prefix test, not a regex.
- **Backend needed:** No.
- **Verify:** Firewall tests: a body whose text part contains a real-shaped secret AND whose image data
  URI contains an `AIza`+24 chars sequence -> the secret is redacted, the data URI is byte-identical,
  `restoreMap` has exactly the text secret; same for Anthropic `source.data`; a body whose only
  candidate matches are inside image data returns `(body, nil)` untouched; existing firewall tests still
  pass; an http image URL with `?token=...` is still scanned. Standard verification. Commit:
  `fix(proxy): never scan or rewrite base64 image payloads in the privacy firewall`.

### I4 Vision override + model attribution on gateway and logs

- **What:** Apply the operator's `vision_model` override on the gateway passthrough, and record the
  model actually sent upstream as `model_used`.
- **Why:** `vision_model` and `nexusImages` only exist in `callUpstreamOnce`, so an image request via
  `/v1/chat/completions` against a provider with `vision_model` silently uses the plain mapped model
  (different behavior from `/v1/messages`). Separately, `logResult`, `recordUsageEvent`, `relayOpenAI`
  log and the passthrough error path use `mappedModel(...)`, which never reflects the vision override,
  so dashboard/usage rows name the wrong model for image requests. Background item 4.
  `StrategyDirect` must stay unchanged: requested model verbatim, no override, images or not.
- **How:** Work in the worktree (see I1). Read `internal/proxy/handler.go` (`mappedModel`,
  `callUpstreamOnce`, `logResult`, `recordUsageEvent`), `internal/proxy/gateway.go`
  (`HandleChatCompletions`, `callOpenAIPassthrough`), `internal/proxy/openai.go` (the `model_used` log
  field in `relayOpenAI`). Then:
  1. Add `func (h *Handler) upstreamModel(active *activeProvider, requestedModel string, images bool) string`:
     direct strategy -> `requestedModel`; else if `images` and `active.impl` implements
     `providers.VisionCapable` with a non-empty `VisionModel(requestedModel)` -> that value; else
     `h.mappedModel(...)`. Keep `mappedModel` as is (other code and the other session's tests may call it).
  2. `callUpstreamOnce`: replace the inline targetModel logic with `h.upstreamModel(active, req.Model, req.nexusImages)`
     (behavior must be identical, including the direct bypass).
  3. `HandleChatCompletions`: set `areq.nexusImages = messagesHaveImage(rawMsgs.Messages)` (from I2);
     give `callOpenAIPassthrough` an `images bool` parameter and use `upstreamModel` for `m["model"]`;
     in its transport-error branch build the `AnthropicRequest{Model: inModel, nexusImages: images}`.
  4. Replace `h.mappedModel(active, req.Model)` with `h.upstreamModel(active, req.Model, req.nexusImages)`
     in `logResult`, `recordUsageEvent`, and the `relayOpenAI` log line. Check `rg -n "mappedModel\("`
     afterwards: remaining uses must be intentional.
  Do not edit `internal/proxy/usage.go` or `internal/proxy/usage_events_test.go` (another session has
  uncommitted changes there in the main checkout; they are not in the worktree, and merging must not
  conflict).
- **Backend needed:** No.
- **Verify:** Tests: gateway, `StrategyAuto`, provider with `vision_model` -> image_url request goes
  upstream with the vision model, text-only request with the mapped model; `StrategyDirect` + image ->
  requested model verbatim; provider without `vision_model` -> mapped model; `/v1/messages` behavior
  unchanged (existing vision tests pass); `upstreamModel` unit test covering the four branches; a log
  test (use the existing sqlite/db helpers in the proxy tests - see how `usage_events_test.go` builds a
  handler with a db, but do not edit that file) asserting `model_used` on the request row equals the
  vision model for an image request. Standard verification. Commit:
  `fix(proxy): apply vision_model on the OpenAI gateway and log the model actually used`.

### I5 Inspector capture omits base64 image payloads

- **What:** When `--inspect` is on, replace base64 image payloads in the captured prompt with a short
  marker.
- **Why:** `logResult` stores `capText(json.Marshal(req))` (64KB cap): one image request stores up to
  64KB of truncated, useless base64 in SQLite. Background item 6. Low severity, small change.
- **How:** Work in the worktree (see I1). Read `logResult` in `internal/proxy/handler.go` and
  `internal/proxy/inspect_test.go`. Add a small helper (e.g. `elideImageData(s string) string`) in
  `internal/proxy/images.go` (created in I2) that replaces the value of `"data":"<long base64>"` with
  `"data":"[image data omitted: N bytes]"` using one compiled regexp for base64 runs of at least ~128
  characters following `"data":"`, and apply it to the marshaled prompt before `capText`. The response
  capture is unchanged.
- **Backend needed:** No.
- **Verify:** Unit test for the helper (short data untouched, long base64 replaced with the byte count,
  non-image text untouched); an inspect-enabled handler test (template: `inspect_test.go`) showing the
  stored prompt for an image request contains the marker and not the base64. Standard verification.
  Commit: `fix(proxy): omit base64 image payloads from inspector prompt capture`.

### I6 End-to-end image test matrix across all gateway paths

- **What:** One new test file that pins the combined behavior across paths, so future refactors cannot
  regress any of them silently.
- **Why:** I1-I5 each have focused tests; the bugs found here were all interaction bugs between layers
  (parser x cache x firewall x provider type x streaming). A table-driven matrix catches those.
- **How:** Work in the worktree (see I1). Create `internal/proxy/image_e2e_test.go` reusing existing
  helpers (`buildTestHandler`, `testProv`, `semanticMock`, `captureModelServer`, `chatCompletions`,
  `doMessages` - read them first; add small local helpers rather than editing shared test files).
  Cases, all asserting the image reached the upstream intact:
  1. Gateway -> OpenAI-compatible, non-stream: data URI + `detail` byte-identical upstream.
  2. Gateway -> OpenAI-compatible, `"stream":true` with an SSE mock: 200, stream relayed, image intact.
  3. Gateway -> Anthropic-format provider: proper base64 image block, no empty text block.
  4. `/v1/messages` -> OpenAI-compatible with an image inside a `tool_result`: upstream gets an
     `image_url` part, and the vision_model override applies.
  5. Failover: first provider returns 500, second provider receives the image intact (both gateway paths
     as applicable).
  6. Exact response cache enabled: identical image request twice -> second is a HIT, upstream called once;
     same text with a different image -> two upstream calls.
  7. Semantic cache enabled: same text, different `image_url` -> never a semantic HIT.
  8. Firewall enabled with an image whose base64 contains `AIza...`: upstream data URI is byte-identical
     while a secret in the text part is still redacted.
  Use `-race`. Do not skip cases silently: if a case cannot be built, report `blocked` with why.
- **Backend needed:** No.
- **Verify:** `go test -race -run 'Image' ./internal/proxy/` passes; then the standard verification for
  the touched packages. Commit: `test(proxy): end-to-end image coverage across gateway paths`.

### I7 Docs: CLAUDE.md, CHANGELOG, provider notes

- **What:** Bring documentation in line with the new behavior; remove statements that are now false.
- **Why:** CLAUDE.md is the project's working memory; its "Vision / Image Content Support" section
  states `hasImageContent` is shallow by design, says `mappedModel` deliberately skips the vision
  override, and describes image handling only for `/v1/messages`. Stale docs mislead the next session.
- **How:** Work in the worktree (see I1). Read the CLAUDE.md sections "Vision / Image Content Support",
  "Direct Strategy (Model-ID Passthrough)" and "Critical Decisions", `CHANGELOG.md` (`## Unreleased`),
  and `rg -n -i "vision|image" README.md docs/*.md` for other mentions. Then:
  1. CLAUDE.md vision section: gateway inbound `image_url` support and strict validation; the shared
     `contentHasImage` recursive detector (replaces the shallow scans, and why); `upstreamModel`
     (vision override on the gateway and in `model_used` logs; `mappedModel` still exists and stays
     image-unaware); firewall skips base64 payloads (and why); inspector elides payloads; Anthropic
     conversion of `image_url` for anthropic/bedrock/vertex; URLs are forwarded as received, no NEXUS
     fetching, provider-dependent URL support; add a short bullet to "Critical Decisions". Keep the
     "do not reintroduce a vision-model catalog" paragraph. Fix any now-false sentence rather than
     appending contradictions.
  2. CHANGELOG `## Unreleased`: one user-facing bullet for gateway image support (what works now, who is
     affected: pi.dev/opencode/Cursor-style OpenAI clients) and one for the firewall/cache/vision fixes.
  3. README/docs: only update where images or vision are already mentioned. Make NO claims about which
     third-party providers support URL images or which image formats they accept: say it is
     provider-dependent and that NEXUS forwards the image as received.
  Docs only; no code changes.
- **Backend needed:** No.
- **Verify:** `rg -n -i "shallow|top-level-only|does not recurse" CLAUDE.md` shows no remaining claim
  about the old scan; links/section names intact; `git diff --stat` shows only docs files. Commit:
  `docs: image support on the OpenAI gateway`.

### V1 Full verification of the committed branch in a clean state

- **What:** Prove the committed branch is correct, formatted, scoped, and race-free, independent of the
  main checkout's uncommitted state.
- **Why:** The main checkout has another session's uncommitted edits; only a clean checkout of the
  branch tells the truth about what will be pushed and deployed.
- **How:** In the worktree (see I1; it is already a clean checkout of the branch plus this untracked
  plan file): run `git status --short` (only the plan file may be untracked), `gofmt -l internal cmd`
  (must print nothing for files changed on this branch; report, do not fix, pre-existing unformatted
  files), `go vet ./...`, `go build ./...`, `go test -race ./...`. Then `git diff --stat main...HEAD`
  (use `git merge-base`/`git diff $(git merge-base main HEAD) HEAD --stat`) and confirm the changed
  files are only the intended ones (no `usage.go`, `usage_events_test.go`, `web/`). Review the full
  diff once for leftovers (debug prints, TODOs, accidental formatting churn). Run
  `go test -race -count=3 -run 'Image|Firewall|Semantic|Gateway' ./internal/proxy/` to catch flakiness.
  Report failures with output; do not paper over them. If something fails, report `blocked` with the
  failing test names; the main agent will route fixes.
- **Backend needed:** No.
- **Verify:** all commands exit 0; diff scope is as intended; report the commit list
  (`git log --oneline main..HEAD`).

### D1 Merge to main, push to opi (needs user decision)

- **What:** Land the branch on `main` and push it to the home git server (`opi`).
- **Why:** The Pi deploys from a git checkout whose `origin` is the same server as local remote `opi`.
- **How:** Main-agent decision gate BEFORE delegating (do not delegate until decided): `main` now contains
  the other session's usage-measurement review commits, including a storage change that switches SQLite to
  WAL mode, and the Pi currently runs 2175f6d (older). Pushing `main` and rebuilding on the Pi would deploy
  all of it together with this fix. Ask the user whether to deploy (a) everything on `main` plus this fix,
  or (b) only this fix on top of what the Pi currently runs (cherry-pick the branch's commits onto 2175f6d
  as a separate branch, verify, push that branch, deploy it). Then, for (a): in the main checkout run
  `git fetch`, rebase the worktree branch onto current `main` if it moved (resolve conflicts, re-run
  V1's commands), `git merge --ff-only fix/gateway-image-support` in the main checkout (the other session's
  uncommitted files must stay untouched; abort if git would overwrite them), then `git push opi main`.
  Never push to `origin` or `fork` (public/third-party remotes) unless the user explicitly asks for that
  remote by name.
- **Backend needed:** No.
- **Verify:** `git log --oneline -3 opi/main` shows the fix commits; `git status` in the main checkout still
  shows only the other session's WIP files.

### D2 Deploy to the Raspberry Pi (docker compose)

- **What:** Update the running NEXUS container on the Pi to the pushed commit.
- **Why:** The user asked for deployment after verification.
- **How:** Facts (verified): SSH alias `rpi` works, login shell is fish (use `ssh rpi bash -s <<'EOF'`).
  Repo on the Pi: `/home/rpi/sources/github.com/nexus-proxy` (clean, `origin` = the opi git server).
  Container `nexus`, compose project `nexus-proxy`, image `nexus-proxy-nexus`, ports 3000/2222, data bind
  mount `/mnt/hdd1/apps/nexus` -> `/home/nexus/.nexus` (config.toml + nexus.db live there; never touch
  or recreate it). Zoraxy (container `zoraxy`) fronts port 80/443. aarch64. Steps:
  1. Preflight on the Pi: `git status --short` must be clean; record `git rev-parse --short HEAD`;
     `mountpoint /mnt/hdd1` and `ls /mnt/hdd1/apps/nexus` must succeed (the external HDD detached earlier
     today; do not deploy onto a missing mount); record `docker ps` state of `nexus`.
  2. Rollback point: `docker tag nexus-proxy-nexus:latest nexus-proxy-nexus:pre-image-fix`.
  3. `git fetch origin && git merge --ff-only origin/<branch pushed in D1>` (or `git pull --ff-only`).
  4. `docker compose up -d --build` (long-running: use a long Bash timeout or background; the build runs
     `npm ci` and a Go build on aarch64).
  5. Wait for health: `curl -s http://localhost:3000/health` returns `{"status":"ok",...}`; `docker ps`
     shows `nexus` Up; `docker logs --tail 50 nexus` has no panics/migration errors; dashboard
     `curl -s http://localhost:2222/api/stats` returns JSON.
  Stop conditions: any preflight failure, a failed build, or an unhealthy container -> do not retry
  blindly; roll back with `docker tag nexus-proxy-nexus:pre-image-fix nexus-proxy-nexus:latest &&
  docker compose up -d --no-build` plus `git reset --hard <recorded HEAD>` on the Pi, then report
  `blocked` with logs. Never run `docker compose down -v` or touch other containers (zoraxy, technitium,
  homeassistant, jmscan).
- **Backend needed:** No.
- **Verify:** new commit hash on the Pi; container healthy; `docker image ls nexus-proxy-nexus` shows both
  `latest` and `pre-image-fix`.

### D3 Live end-to-end image test through the real proxy

- **What:** Prove, against the deployed instance, that the original failure is fixed.
- **Why:** Unit tests with mocks cannot prove the real Ollama Cloud path works.
- **How:** Work from the main checkout directory but only run read-only/ network commands. Use the
  image `docs/social-preview.png` (49 KB) from the repo. Build request JSON files in the scratchpad
  directory. Replicate pi's request shape: `POST /v1/chat/completions`, model `kimi-k3`, one user message
  with a text part and an `image_url` part (data URI). Run each of these against BOTH
  `http://nexus.home.com/v1` (port 80 via Zoraxy, what pi uses) and `http://nexus.home.com:3000/v1`:
  1. Non-stream image request -> expect HTTP 200 and an answer that describes the image (it contains
     visible text; quote what the model read). Previously a 400.
  2. Same with `"stream":true` -> SSE chunks arrive and end with `[DONE]`.
  3. Text-only regression request -> 200.
  4. An Anthropic-shape image request to `POST /v1/messages` (model `claude-sonnet-4-6`, an `image`
     block with a base64 source) -> a sensible response or a faithful relay of the provider's own error.
  5. Negative: an `image_url` with an invalid scheme (`ftp://x`) -> 400 with the new clear message.
  6. Check the request log/usage via the dashboard API (`http://nexus.home.com:2222/api/requests`,
     `/api/usage/events?limit=5`): the image requests are recorded with the expected `model_used`.
  7. Optional, bounded (max ~5 tool calls): try pi itself with the vision model on the image
     (`pi --help` to find the non-interactive flags). Do not edit any pi config files. If pi cannot be
     driven non-interactively, say so and skip.
  Report exact HTTP statuses and the model's answer. If Ollama Cloud itself rejects the image or the
  model, report that as an upstream limitation with its error text (not as a NEXUS failure), and state
  what was verified up to that point (NEXUS forwarded the image part intact).
- **Backend needed:** Needs the Pi up (verified in D2).
- **Verify:** items 1, 2 and 5 pass; items 3, 4, 6 are consistent; summary of evidence returned.

## Completion Log

| Date | Item | Notes |
|------|------|-------|
| 2026-10-04 | I1 | Commit 7b9c2e1. Parser accepts validated image_url parts; Anthropic() emits image blocks; MarshalJSON keeps images. Anthropic-format gateway test uses a bedrock provider (the anthropic provider's base URL is hardcoded). Main agent re-verified scope (3 files), build, proxy tests. gofmt flags only pre-existing unformatted code in transformer.go. |
| 2026-10-04 | I2 | Commit f6e0670. New images.go (contentHasImage, messagesHaveImage; image/image_url/input_image; recurses into tool_result content, depth cap 8); hasImageBlock and hasImageContent deleted. Main agent mutation-checked: with the old semantic.go/handler.go restored, the 3 new integration tests fail; with the new code they pass. |
| 2026-10-04 | I3 | Commit 42dcc64. Firewall walk skips Anthropic source.data (type base64) and base64 data URIs (prefix test, 128-byte window); http(s) URLs still scanned; import order fixed in firewall.go. Sub-agent confirmed the new tests fail on the old firewall (extra APIKEY redactions from AKIA/AIza in image data, modified image-only bodies); main agent re-verified scope (2 files) and tests. |
| 2026-10-04 | I4 | Commit 8de8d2a. New Handler.upstreamModel (direct -> requested id; image + OpenAI-compatible + vision_model -> override; else mappedModel) used by callUpstreamOnce, gateway passthrough (new images param, nexusImages via messagesHaveImage), logResult, recordUsageEvent, relayOpenAI log. Deviation from plan, accepted after diff review: the vision branch also requires IsOpenAICompatible so model_used never names a model that was not sent to an Anthropic-native provider. mappedModel kept. New upstream_model_test.go (own sqlite helper; usage_events_test.go untouched). Sub-agent confirmed gateway-vision and model_used log tests fail with the change reverted; direct strategy tests pass both ways. |
| 2026-10-04 | I5 | Commit de3b101. elideImageData (one compiled RE2 regexp, Contains pre-check) applied before capText in logResult; unit test + inspect-enabled handler test (fails without the change, per sub-agent). Main agent re-verified scope (4 files) and tests. |
| 2026-10-04 | I6 | Commit a7e4ff3. image_e2e_test.go: 8 TestImageE2E_* cases across gateway/messages, stream, Bedrock, failover, exact+semantic cache, firewall; all pass with -race and -count=3. Main agent mutation-checked the matrix: disabling the firewall data-URI skip fails the firewall case; disabling contentHasImage fails the tool_result-vision and semantic-cache cases; restored and clean. |
| 2026-10-04 | I7 | Commit b0a7790 (CLAUDE.md, CHANGELOG.md only). Vision section rewritten (shared detector, upstreamModel, gateway image_url validation, Anthropic conversion, URLs-not-fetched, firewall, inspector), Direct Strategy section de-staled (dropped stale line refs in the rewritten paragraph), Critical Decisions bullet added (records reversal of "shallow by design"), two CHANGELOG bullets. Main agent read the full diff: accurate against the code. README/docs had no image mentions. |
| 2026-10-04 | V1 | Clean state verified: go vet ./..., go build ./..., go test -race ./... all pass (8 packages); -count=3 on Image/Firewall/Semantic/Gateway shows no flakes; git diff --check clean; diff scope = 16 files, none of usage.go / usage_events_test.go / web/; no debug prints or TODOs. gofmt lists 20 pre-existing unformatted files, of which only transformer.go is touched by this branch (its unformatted lines predate the branch and are not in its hunks). main tip still 643450b at that time. |
