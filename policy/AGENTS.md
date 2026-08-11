# policy — x-agentic-access governance layer

> **STATUS: Work-in-Progress (PoC).** This is an observability-first proof of
> concept, **not production-ready**. The foundation (vocabulary, decorator,
> audit logging, egent integrations) is functional in observe mode only.
> Several critical enforcement components are still WIP — see
> [What's still WIP](#whats-still-wip) below. Do **not** flip
> `POLICY_ENFORCE_*` in production until those are resolved.

`egent-common/policy` is a per-operation governance layer that wraps every Eino
`tool.InvokableTool` so an AI agent cannot invoke a tool without passing a
server-side policy gate. Modeled on Composio's published `x-agentic-access`
contract (`composio-api-evangelist/agentic-access/composio-agentic-access.yml`).

## Why it exists

Without this layer, any tool the LLM picks runs unconditionally. An agent can
`rm -rf`, send email, charge money, or submit authenticated forms — all with
zero server-side check. The policy layer puts a deterministic, non-LLM gate in
front of every tool call:

- **Visibility** — every policy-gated call emits a structured `Decision` audit
  record (tool, actor, action class, consequence, verdict, reason, args
  redacted).
- **HITL** — destructive / high-value / safety-critical calls require
  human-in-the-loop sign-off before they execute.
- **Authorization** — mutating calls require an authenticated subject; a
  Authorizer can deny per (subject, tool, policy).
- **Fail-safe** — on infra error, safety-critical calls fail-closed, read-only
  calls fail-open.

## Design principle: observability-first

The layer ships in **observe mode**. Audit always fires; enforcement (HITL
blocks, authz denials) is opt-in via env vars so the layer can land in
production without changing any tool's runtime behaviour:

| Env var | Default | Effect when `1`/`true` |
|---|---|---|
| `POLICY_ENFORCE_HITL` | `false` | HITL gate returns a synthetic `[awaiting approval …]` content string instead of letting the call through |
| `POLICY_ENFORCE_AUTHZ` | `false` | Authorization denials block the call instead of just logging |

This means: flip both to `1` when ready, watch the audit log in observe mode
first to confirm the trigger/HITL classifications are correct, then enforce.

## Vocabulary — `XAgenticAccess`

Every tool carries one `XAgenticAccess` policy (`policy.go:82`):

| Field | Values | Meaning |
|---|---|---|
| `ActionClass` | `connected` / `acting` | passive read vs mutating side-effect |
| `Consequence` | `read` / `write` / `safety-critical` | blast radius; safety-critical uses fail-closed + short-TTL tokens |
| `Subject` | `optional` / `required` | whether an authenticated actor must be in the request context |
| `HITL` | `none` / `conditional` / `required` | human-in-the-loop gate |
| `Audit` | `none` / `required` | whether each invocation emits a `Decision` record |
| `Triggers` | `[]string` | conditional-HITL trigger set: `abnormal`, `high-value`, `destructive` |

### How to pick values for a new tool

| Tool type | ActionClass | Consequence | Subject | HITL | Audit |
|---|---|---|---|---|---|
| Pure read (GET, search, extract, calculator) | `connected` | `read` | `optional` | `none` | `none` |
| Local workspace mutation (create file, run sandboxed code) | `acting` | `write` | `required` | `none` | `required` |
| Charged / cross-tenant (API calls that cost money or move data) | `acting` | `write` | `required` | `conditional` (`high-value`) | `required` |
| Connector (Composio execute) | `acting` | `write` | `required` | `conditional` (`destructive`,`high-value`) | `required` |
| Host shell / real browser | `acting` | `safety-critical` | `required` | `required` | `required` |

When in doubt: **default to the conservative bucket** (`acting` / `write` /
`required` / `none` / `required`) so a new tool is never silently un-governed.
A read tool that's accidentally wrapped as write is just noisy; a mutating
tool wrapped as read is a security hole.

### Inference helpers

`InferFromHTTPMethod(method)` returns a conservative default from the HTTP verb
alone — GET/HEAD → connected/read, everything else → acting/write+conditional
HITL. `Resolve(explicit, method)` merges an explicit policy with inferred
defaults, patching zero-valued fields so partial YAML declarations still
produce a complete policy. This is what category YAMLs use: any tool without an
explicit `x-agentic-access:` block still gets a sensible policy.

## Enforcement order — `middleware.go:50` (Eino-native) / `decorator.go:83` (legacy)

The enforcement point is `PolicyMiddleware.WrapInvokableToolCall` (Eino-native
`ChatModelAgentMiddleware`). The legacy `policyCheckedTool.InvokableRun`
decorator is superseded but retained in `decorator.go` for backward compat.
Both enforce the same order:

1. **Subject check** — `subject=required` and no actor in context → deny
   (`[policy: denied …]` content, no error).
2. **HITL gate** — `required` always fires; `conditional` fires only when one
   of the policy's declared triggers is present in the request context (set via
   `WithTriggers`). In enforce mode returns `[awaiting approval …]`; in observe
   mode logs `hitl_pending_observed` and lets it through.
3. **Authorize** — only when an `Authorizer` is wired AND `EnforceAuthz()`.
   Deny → block; error → fail-closed for safety-critical, fail-open otherwise
   (mirrors `plano-usage.CheckBalance` posture).
4. **Delegate** to the inner tool.
5. **Audit** — when `Audit == required`, emit a `Decision` with the outcome
   (allow/error) and redacted args.

All non-fatal policy decisions return `(string, nil)` — matching the codebase's
error-as-content convention (see `egent-public-apis/tool/api_tool.go:144-146`)
so the ReAct loop recovers instead of aborting the whole turn.

### Fail-safe rules

- `IsMutating()` → fail-closed on authz infra errors.
- `IsSafetyCritical()` → always fail-closed on any infra error.
- Read-only tools → fail-open (infra error doesn't block the call).

## Auditor — `audit.go`

`Auditor` is the sink for `Decision` records. Implementations must be
concurrency-safe and non-blocking.

- **`SlogAuditor`** — structured JSON via `slog`. This is the current PoC sink.
- **`NoopAuditor`** — discards everything. Default when no auditor is in the
  context (unit tests, `cmd/genlist`).
- **Production target** — a Postgres-backed auditor mirroring
  `plano-usage.Record → Talos` (planned).

### Context plumbing

The auditor flows through the request context, mirroring the codebase's
existing identity-in-context convention (`usage.WithActorID`,
`memory.WithUserID`):

```go
// in main():
policyAuditor = policy.NewSlogAuditor(slog.Default())

// in the HTTP handler, per request:
ctx = policy.WithAuditor(ctx, policyAuditor)
ctx = usage.WithActorID(ctx, actorIDFromHeader)
ctx = policy.WithTriggers(ctx, triggersForThisCall) // optional
```

`AuditorFromContext(ctx)` returns the auditor or a `NoopAuditor` — never nil.

## Integration pattern (how to wire a new egent)

### Primary: Eino-native PolicyMiddleware (recommended)

1. **Define policy buckets** for your toolset (or reuse the generic buckets
   below). Put them in a `policy.go` next to your `main.go`:

   ```go
   var (
       policyRead = policy.XAgenticAccess{
           ActionClass: policy.ActionConnected,
           Consequence: policy.ConsequenceRead,
           Subject:     policy.SubjectOptional,
           HITL:        policy.HITLNone,
           Audit:       policy.AuditNone,
       }
       policyMutating = policy.XAgenticAccess{
           ActionClass: policy.ActionActing,
           Consequence: policy.ConsequenceWrite,
           Subject:     policy.SubjectRequired,
           HITL:        policy.HITLNone,
           Audit:       policy.AuditRequired,
       }
   )
   ```

2. **Classify each tool by name** (a toolset with mixed read/mutating tools
   needs per-name classification, like `egent-crew/policy.go:policyForCrewTool`):

   ```go
   func policyForTool(name string) policy.XAgenticAccess {
       switch name {
       case "get_x", "search_y":
           return policyRead
       case "create_z", "send_w":
           return policyMutating
       }
       return policyMutating // conservative default
   }
   ```

3. **Build a PolicyRegistry** at startup and create the middleware:

   ```go
   registry := policy.NewRegistry(policyMutating) // default for unknown tools
   for _, t := range tools {
       info, _ := t.Info(ctx)
       registry.Register(info.Name, policyForTool(info.Name))
   }
   middleware := policy.NewMiddleware(registry, nil) // nil = no authorizer yet
   ```

4. **Attach to the agent's Handlers** (after context middlewares):

   ```go
   handlers := append(contextMiddlewares, middleware)
   agent, _ := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
       Handlers: handlers,
       // ...
   })
   ```

5. **Initialize the auditor** in `main()` and inject it per request:

   ```go
   policyAuditor = policy.NewSlogAuditor(slog.Default())
   // … later, in the handler:
   ctx = policy.WithAuditor(ctx, policyAuditor)
   ```

6. **Verify** the startup log shows:
   `policy: x-agentic-access layer armed (observe mode, middleware wired)`.

### Legacy: per-tool policy.Wrap decorator (removed)

The old approach wrapped each `tool.InvokableTool` individually via
`policy.Wrap(tool, p, nil)`. This has been **fully removed** from all 3 egents
(2026-08-03). The `PolicyMiddleware` is now the sole enforcement point. The
legacy `decorator.go` code is retained in `egent-common/policy` for reference
but is no longer called from any production path.

### Existing integrations

| Egent | Approach | Classifier | Notes |
|---|---|---|---|
| `egent-public-apis` | **Middleware** (`PolicyMiddleware` on agent Handlers) | `policy.Resolve` from YAML config | Registry built from `cfg.Tools` + connector + knowledge tools |
| `egent-crew` | **Middleware** (`PolicyMiddleware` on each persona agent) | `policyForCrewTool` (per-name switch) | Mixed read/mutating per persona; host shell + browser → `safety-critical` + HITL required |
| `egent-jigsawstack` | **Middleware** (`PolicyMiddleware` on agent Handlers) | `policyForJigsawTool` (inline in `NewAgent`) | `image_generation`/`tts` → charged; rest → read-only |

## Web / client integration

### Current state (observe mode): backend-only

No `web/` changes needed. The policy layer returns plain text strings
(`[policy: denied …]`, `[awaiting approval …]`) that render as regular
assistant content. Audit records go to slog (server-side). The client is
unaware the policy layer exists.

### Enforce mode: client integration required (but UI primitives already exist)

Flipping to enforce mode requires the HITL approval roundtrip to reach the
client. The good news: **the web client already has the UI primitives** — the
gap is on the backend side (egent must emit proper AI SDK tool states, not
plain strings).

| Layer | What's needed | Status |
|---|---|---|
| **web `ai-elements/tool.tsx:56`** | State machine already handles `approval-requested`, `approval-responded`, `output-denied`, `output-error` with labels + icons | **Already built** |
| **web `ai-elements/confirmation.tsx`** | `ConfirmationActions` (Approve/Reject buttons), `ConfirmationAccepted`/`ConfirmationRejected` conditional rendering | **Already built** |
| **web `chat-view.tsx`** | Compose the `Confirmation` component into the tool renderer for approval-gated tools | **Not wired** (small change once backend emits the state) |
| **web `plano-transport.ts`** | Handle the approve/deny roundtrip (send approval response back to egent) | **Not wired** |
| **egent backend** | Emit AI SDK tool state `approval-requested` instead of returning a synthetic `[awaiting approval …]` string; block until client responds or timeout | **Not built** (current `hitlPendingContent()` returns a plain string) |
| **egent backend** | Resume mechanism — after approval, re-invoke the gated tool and continue the turn | **Not built** |

**Bottom line:** the web UI for HITL is already built and sitting unused. The
work is primarily backend (egent tool-state emission + resume) with a small
transport + renderer wiring on the client side.

## Priority assessment

**Medium, not high.** Rationale:

- **No active threat today.** The egents run behind plano's auth gateway
  (Kratos/Oathkeeper). A caller must already be authenticated to reach an egent.
  The policy layer adds defense-in-depth, not the only barrier.
- **Observe mode already delivers value.** Every tool call is audited — that's
  useful for debugging, compliance, and incident reconstruction right now,
  without any enforcement risk.
- **Enforcement is high-risk to flip on.** False-positive HITL blocks or authz
  denials would break the agent loop in production. Needs careful tuning of
  trigger thresholds + a real approval surface before it's safe.
- **The expensive missing pieces (Postgres auditor, authorizer, HITL
  resume) are infrastructure projects**, not quick wins. Each is a distinct
  workstream.

**When this becomes high priority:**
- Agents get direct access to payment/financial tools (charge, refund, transfer)
- Agents get write access to shared/multi-tenant data without a human in the loop
- Compliance/SOC2 requires auditable tool-access controls
- Multiple users share an agent instance with different permission levels

Until then, observe mode is the right operating point — ship the audit trail,
watch what the agents actually do, and build enforcement when the threat model
demands it.

## What's still WIP

This is a **PoC**. The foundation below works in observe mode, but production
enforcement is blocked on these unresolved items:

| # | Component | Current state | What's missing |
|---|---|---|---|
| 1 | **Postgres auditor** | `SlogAuditor` writes `Decision` records to stderr only — they vanish on restart | A Postgres-backed auditor mirroring `plano-usage.Record → Talos` (persist, query, alert) |
| 2 | **Authorizer** | `PolicyMiddleware` passes `nil` authorizer everywhere | Wire a pREST-backed authorizer. Without this, `POLICY_ENFORCE_AUTHZ=1` has no effect — there's nothing to deny |
| 3 | **Trigger detection** | `WithTriggers` is never called in any request path | Implement signal detectors that populate `WithTriggers` per call (off-hours, geo, cost threshold, destructive arg shape). Without this, conditional HITL (`high-value`, `destructive`, `abnormal`) **never escalates** — only `HITLRequired` tools (host shell/browser) would block |
| 4 | **HITL approval surface** | Enforce mode returns a synthetic `[awaiting approval …]` string | No UI/workflow to actually approve or resume. In enforce mode the agent just gets stuck with no path forward |
| 5 | **Enforce-mode testing** | `POLICY_ENFORCE_*` never flipped with real traffic | Load test observe-mode audit → confirm classifications correct → flip enforce → verify no false-positives break the agent loop |

**What works now (observe mode):** every policy-gated tool call emits a
structured `Decision` log (tool, actor, verdict, reason, redacted args). Nothing
is blocked or gated.

**What does NOT work now (enforce mode):** flipping `POLICY_ENFORCE_*` would
only affect `HITLRequired` tools (host shell/browser) — and those have no
approval surface, so the agent would stall. Authz enforcement is a no-op
(nil authorizer). Conditional HITL is a no-op (no triggers).

**Bottom line:** safe to run as-is (observe mode). Not safe to enforce in
production until items 1–4 are resolved.

## Roadmap

| Phase | Status | What |
|---|---|---|
| PoC: observe-mode audit + policy vocabulary | done | this package |
| egent-crew + egent-jigsawstack wiring (legacy `policy.Wrap`) | done | per-name classifiers, auditor injection |
| **Phase 0: Eino-native PolicyMiddleware** | **done** | `PolicyRegistry` + `PolicyMiddleware` wired into all 3 egents |
| Remove legacy `policy.Wrap` from egent-public-apis | **next** | `tool_builder.go` + `sharedtools.go` still use the decorator (redundant but safe) |
| Postgres-backed auditor → Talos | planned | mirror `plano-usage.Record` shape |
| Keto-backed `Authorizer` wiring | planned | Wire a pREST-backed authorizer (currently `nil`) |
| Trigger detection (abnormal/high-value/destructive) | planned | populate `WithTriggers` per request from signal detectors |
| Enforce mode flip | planned | `POLICY_ENFORCE_HITL=1` + `POLICY_ENFORCE_AUTHZ=1` in prod after observe-mode audit confirms classifications |

## Files

- `policy.go` — `XAgenticAccess` vocabulary, `InferFromHTTPMethod`, `Resolve`,
  `IsMutating`/`IsSafetyCritical`.
- `registry.go` — `PolicyRegistry`: thread-safe map of tool name → `XAgenticAccess`.
- `middleware.go` — `PolicyMiddleware`: Eino `ChatModelAgentMiddleware` that enforces
  policies from the registry on every tool call.
- `decorator.go` — **legacy** `policyCheckedTool` (the `Wrap` result), enforcement order,
  `Authorizer` interface, `WithTriggers`. Superseded by `middleware.go`.
- `audit.go` — `Decision`, `Auditor`, `SlogAuditor`, `NoopAuditor`,
  `WithAuditor`/`AuditorFromContext`, `EnforceHITL`/`EnforceAuthz` env gates.
- `policy_test.go` — table tests covering each verdict path (legacy decorator).
- `middleware_test.go` — 15 tests covering registry defaults, read pass-through,
  subject deny/allow, HITL observe/enforce/conditional, authz allow/deny/
  fail-closed/fail-open, args passthrough, error propagation.
