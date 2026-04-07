# Plan Replayer Explain Admin

This ExecPlan is a living document. Keep `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` up to date as work proceeds.

Reference: `PLANS.md` at repository root; this plan is maintained according to it.

## Purpose / Big Picture

After this change, TiDB can grant a dedicated dynamic privilege, `PLAN_REPLAYER_EXPLAIN_ADMIN`, to a service account that needs to run `PLAN REPLAYER DUMP EXPLAIN` for read-only debugging workflows without granting broad table read privileges. The behavior stays narrow: only internal `EXPLAIN <SELECT / set-op>` and the matching `SHOW CREATE TABLE/VIEW` metadata fetches can bypass object privilege checks, while `EXPLAIN ANALYZE`, write statements, and normal SQL continue to use the existing privilege model.

## Progress

- [x] (2026-04-01 13:20 +08:00) Located the real privilege enforcement points in the plan replayer dump path and confirmed that the outer `PLAN REPLAYER` statement does not pre-check inner SQL object privileges.
- [x] (2026-04-01 14:05 +08:00) Added the session-scoped internal privilege context and initially wired it into the bound privilege manager.
- [x] (2026-04-01 14:20 +08:00) Registered the new dynamic privilege and wrapped the internal `EXPLAIN` / `SHOW CREATE` execution points.
- [x] (2026-04-02 11:30 +08:00) Simplified the session state from a struct to a single temporary internal marker and documented why bypass is decided at task granularity.
- [x] (2026-04-02 12:05 +08:00) Renamed the helper to match the temporary bypass mark and simplified `ExplainNonEvaledSubQuery` switching to only toggle when needed.
- [ ] Update or add tests.
- [ ] Run targeted validation and capture evidence.

## Surprises & Discoveries

- Observation: `PLAN REPLAYER DUMP` writes schema metadata before it executes internal `EXPLAIN`.
  Evidence: `pkg/domain/plan_replayer_dump.go` calls `dumpSchemas(...)` before `dumpPlanReplayerExplain(...)`.

- Observation: view-related explain privilege failures have a dedicated error path (`ErrViewNoExplain`), so the bypass must hook the regular privilege manager instead of only handling generic privilege errors.
  Evidence: `pkg/planner/core/logical_plan_builder.go` directly calls `RequestVerification(...)` during view expansion when `StmtCtx.InExplainStmt` is true.

## Decision Log

- Decision: Keep a session-scoped internal mark, but only activate it when the normal helper SQL path fails with a privilege error and the task qualifies for `PLAN_REPLAYER_EXPLAIN_ADMIN`.
  Rationale: this preserves existing-user behavior while still letting the new dynamic privilege rescue privilege failures for the intended read-only helper SQL shapes. The final form is a temporary boolean mark stored on `sctx`, not a dedicated `SessionVars` field.
  Date/Author: 2026-04-01 / Codex

- Decision: Consume the bypass in planner privilege-check paths instead of hacking `pkg/privilege/privileges`.
  Rationale: reviewer feedback requires keeping the privilege package generic. The actual bypass now happens where planner consumes current-user privilege checks: `CheckPrivilege(...)` for `visitInfo`-based checks and the direct nested-view / fast-path `pm.RequestVerification(...)` call sites. The planner-side helper only consults the temporary `sctx` boolean mark plus `PLAN_REPLAYER_EXPLAIN_ADMIN`, without re-encoding privilege-bit shapes. `visitInfo` generation remains intact so view-definer validation still sees the collected privilege shape.
  Date/Author: 2026-04-01 / Codex

- Decision: Support file-input mode with the same task-wide allowlist as inline and statement-list inputs.
  Rationale: once the file contents are parsed into `ExecStmts`, the dump path is shared, so a separate execution-mode gate is unnecessary; the actual bypass decision should depend on `ANALYZE`, statement types, and the dynamic privilege check only.
  Date/Author: 2026-04-01 / Codex

- Decision: Keep the bypass decision at task granularity instead of per statement.
  Rationale: schema extraction works on the task-wide statement set, so a mixed task must not let unsupported statements indirectly gain `SHOW CREATE` bypass through shared metadata collection.
  Date/Author: 2026-04-02 / Codex

## Outcomes & Retrospective

The source implementation is complete. The latest cleanup passes renamed the temporary helper-state wrapper to match the boolean bypass mark, kept the scalar-subquery safeguard inline while avoiding unnecessary state churn when the session is already on the default safe setting, removed the redundant `AllowExplainAdmin` state after file-input mode started sharing the same task-wide allowlist, tightened the bypass so it only retries after privilege failures for real `SELECT` / set-operator statements, and moved the bypass out of the privilege manager into planner-side privilege consumption.

## Context and Orientation

`pkg/executor/plan_replayer.go` owns the executor-side dump request object. It turns the parsed outer statement into a `domain.PlanReplayerDumpTask`. `pkg/domain/plan_replayer_dump.go` writes the zip contents and issues the internal helper SQL statements: `EXPLAIN`, `SHOW CREATE TABLE`, and `SHOW CREATE VIEW`. `pkg/sessionctx/context.go` provides the temporary `sctx` key used to mark the retry helper SQL window. `pkg/privilege/privileges/privileges.go` remains the generic current-user privilege manager implementation.

In this repository, a "dynamic privilege" is a privilege stored in `mysql.global_grants` and checked by name rather than by a static privilege bit. TiDB currently treats most dynamic privileges as globally grantable and lets `SUPER` work as a fallback, so `PLAN_REPLAYER_EXPLAIN_ADMIN` follows the same built-in privilege model.

## Plan of Work

Add the new dynamic privilege constant and register it in the built-in dynamic privilege list. Record the helper retry window with a temporary boolean mark on `sctx` instead of a dedicated `SessionVars` field. Planner privilege-consumption paths consult that mark together with `PLAN_REPLAYER_EXPLAIN_ADMIN` so the current-user privilege checker remains generic.

On the plan replayer side, let manual, list-based, and file-input dump tasks share the same parsed-statement path. Wrap internal `EXPLAIN` and `SHOW CREATE` execution in helpers that temporarily enable the session context only for fallback retries after privilege errors. Only allow the bypass helper for non-`ANALYZE` tasks whose inner statements are all plain read-only `SELECT` statements; set operations are allowed only when every leaf operand is a real `SELECT`, and `TABLE` / `VALUES`, locking reads, and `SELECT INTO` stay outside the allowlist even when nested inside `UNION` / `INTERSECT` / `EXCEPT`, derived tables, subqueries, or CTEs. While the bypass context is active for `EXPLAIN`, temporarily disable non-evaluated scalar subquery explain output so the plan output does not embed real scalar values.

## Concrete Steps

All commands are run from repository root:

    cd /Users/cryo/project/tidb
    rg -n "PlanReplayer|RequestVerification|InPlanReplayer" pkg

Implementation editing touches only source files and this ExecPlan. Validation is intentionally deferred in this execution session.

## Validation and Acceptance

Acceptance after local validation should show:

1. A user with only `PLAN_REPLAYER_EXPLAIN_ADMIN` can run `PLAN REPLAYER DUMP EXPLAIN SELECT ...`.
2. The same user still cannot run ordinary `EXPLAIN SELECT ...` outside plan replayer.
3. The same user cannot run `PLAN REPLAYER DUMP EXPLAIN ANALYZE SELECT ...`.
4. The same user cannot use the new privilege to make `PLAN REPLAYER DUMP EXPLAIN INSERT ...` succeed.
5. Existing users with ordinary table privileges still keep the old behavior.

## Idempotence and Recovery

All code edits are ordinary source changes and can be safely revised or rerun. If the planner-side bypass is found to be too broad during validation, the rollback path is to remove the planner-side skip logic and the two plan replayer wrappers, which cleanly restores the old behavior.

## Artifacts and Notes

No command output is recorded yet because validation was intentionally deferred.

## Interfaces and Dependencies

The key new internal interfaces are:

- `pkg/privilege.PlanReplayerExplainAdminPriv`, the canonical privilege name.
- `pkg/sessionctx.PlanReplayerPrivilegeBypass`, the temporary `sctx` key used only around helper SQL retry windows.

The main touched functions are:

- `pkg/domain.dumpSchemas`
- `pkg/domain.getShowCreateTable`
- `pkg/domain.dumpExplain`
- `pkg/planner/core.CheckPrivilege`

Updated 2026-04-02: renamed the helper wrapper, simplified the non-evaluated scalar subquery toggle logic, removed the redundant `AllowExplainAdmin` task flag, and changed the bypass path to retry only after privilege failures for real `SELECT` / set-op statements.
