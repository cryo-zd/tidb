# Integration Test Cleanup Review

## Scope

- Reviewed `tests/integrationtest/t/**` with a static, high-signal scan.
- Goal: find cases that mutate session/global state and do not restore it cleanly.
- This report is intentionally not exhaustive. I prioritized:
  - global-state mutations that can leak across `.test` files;
  - `sql_mode` leftovers, because they are easy to miss and can change later statements in the same file.

## Notes on impact

- `mysql-tester` creates a fresh connection/database per `.test` file, so plain session-state leftovers usually do **not** leak across files.
- That means most session-level findings below are hygiene problems:
  - they can create hidden coupling for later statements appended to the same file;
  - they make the file harder to extend safely.
- Global-state leftovers are more important because they can affect later `.test` files in the same runner process.

## Findings

### Higher priority: global state not restored

1. `t/session/temporary_table.test`
   - Lines: 64-67
   - State change: `set @@global.tidb_tmp_table_max_size = 123;` and related session overrides
   - Problem: the global `tidb_tmp_table_max_size` change is not restored later in the file.
   - Why it is worth fixing: this can leak into later files and make temporary-table size behavior depend on execution order.

### Lower priority: session `sql_mode` left dirty

3. `t/expression/builtin.test`
   - Lines: 1654, 1665, 1667
   - State change: switches to `NO_ZERO_IN_DATE`, briefly to `''`, then back to `NO_ZERO_IN_DATE`
   - Problem: no `set sql_mode = default;` after the block.
   - Impact: current tail statements do not seem sensitive, but later additions in the same file would inherit the non-default mode.

4. `t/expression/misc.test`
   - Line: 417
   - State change: `set @@sql_mode = '';`
   - Problem: no restore later in the file.
   - Impact: hidden same-file coupling after the enum-update case.

5. `t/explain_generate_column_substitute.test`
   - Line: 10
   - State change: `set @@sql_mode = "";`
   - Problem: no restore by EOF.
   - Impact: low immediate risk today, but the file keeps a non-default `sql_mode` for all later cases.

6. `t/new_character_set.test`
   - Lines: 71-73, 93
   - State change: the file first restores `sql_mode`/charset vars, but later does `set SESSION sql_mode = '';`
   - Problem: that later `sql_mode` change is never restored by EOF.
   - Impact: easy to miss because there is an earlier cleanup block.

7. `t/new_character_set_builtin.test`
   - Line: 2
   - State change: `set @@sql_mode = '';`
   - Problem: no restore anywhere in the file.
   - Impact: entire file runs under non-default `sql_mode`, and future appended cases would inherit it.

8. `t/new_character_set_invalid.test`
   - Line: 32
   - State change: trailing `set @@sql_mode = '';`
   - Problem: the file ends immediately after switching away from default.
   - Impact: same-file hygiene issue; easy one-line cleanup.

9. `t/ddl/column.test`
   - Line: 23
   - State change: `set @@sql_mode = '';`
   - Problem: no restore by EOF.
   - Impact: the file is short, but it still leaves a non-default session mode for everything after `TestIssue52972`.

10. `t/ddl/column_type_change.test`
    - Lines: 2216, 2223, 2231
    - State change: switches to `ALLOW_INVALID_DATES`, then `ALLOW_INVALID_DATES,STRICT_TRANS_TABLES`, then `''`
    - Problem: the file ends without restoring `sql_mode`.
    - Impact: clear tail leftover; worth normalizing.

11. `t/expression/time.test`
    - Lines: 370-404
    - State change: several explicit composite `sql_mode` assignments, ending with `ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION`
    - Problem: the file ends under a non-default composite mode.
    - Impact: low immediate risk now, but it is the same pattern as the `builtin.test` case you noticed.

12. `t/planner/core/tests/prepare/prepare.test`
    - Line: 432
    - State change: `set sql_mode = "";`
    - Problem: no restore through EOF.
    - Impact: prepared-statement cases after this point implicitly depend on the altered mode.

### Additional categories worth watching

These are not the same as `sql_mode`, but they have the same cleanup smell: a file tweaks the session/runtime environment for one scenario and leaves later statements in that modified environment.

13. `t/executor/executor.test`
    - Lines: 2490, 2562, 2590, 2600, 2623
    - State change:
      - `set global tidb_txn_mode=''`
      - later `set global tidb_txn_mode=pessimistic`
      - separate session-level `set session tidb_txn_mode=''` / `pessimistic`
    - Problem: both global and session `tidb_txn_mode` changes remain in effect after the target cases.
    - Why it is worth fixing: transaction-mode leftovers are easy to miss and can change locking / `for update` semantics for later cases.

14. `t/session/txn.test`
    - Lines: 2, 11, 21
    - State change: session `tidb_txn_mode` is switched and the file ends with `pessimistic`.
    - Problem: no restore to default / previous mode.
    - Impact: same-file coupling for any future transaction tests appended to the file.

15. `t/planner/core/integration.test`
    - Line: 2308
    - State change: `set tidb_opt_fix_control='44262:ON';`
    - Problem: the fix-control toggle is not reset later in the file.
    - Why it is worth fixing: this is close to the "blacklist/whitelist not restored" category you mentioned; later cases may silently inherit a non-default optimizer behavior gate.

16. `t/planner/core/integration_partition.test`
    - Lines: 517, 530, 753, 817
    - State change: repeated `tidb_partition_prune_mode = 'dynamic'`
    - Problem: the file ends without restoring partition prune mode.
    - Impact: partition-planner cases appended later would run under forced dynamic pruning unless they reset it themselves.

17. `t/planner/core/partition_pruner.test`
    - Lines: 5, 48, 51, 883, 956 and other nearby toggles
    - State change: the file repeatedly flips `tidb_partition_prune_mode` between `static` and `dynamic`, and ends with a dynamic setting.
    - Problem: no final restore to default / previous mode.
    - Impact: same-file coupling for later partition-pruner cases; this file is especially easy to drift because it contains many localized mode switches.

18. `t/planner/funcdep/only_full_group_by.test`
    - Line: 2
    - State change: `set @@session.tidb_enable_new_only_full_group_by_check = 'on';`
    - Problem: no restore by EOF.
    - Impact: later additions to the file would implicitly rely on the new checker being enabled.

19. `t/explain.test`
    - Lines: 12-13
    - State change: `tidb_hashagg_partial_concurrency = 1` and `tidb_hashagg_final_concurrency = 1`
    - Problem: the file does not restore these execution knobs.
    - Impact: explain-only files often accrete cases over time; leaving explicit concurrency settings behind makes later plan output more order-dependent.

20. `t/index_join.test`
    - Lines: 8-9, 21-24
    - State change:
      - hash-agg concurrency knobs set to `1`
      - `tidb_opt_insubq_to_join_and_agg` toggled and left at an explicit value rather than restored
    - Problem: no cleanup to default / previous values.
    - Impact: hidden planner-environment coupling across unrelated cases in the file.

21. `t/subquery.test`
    - Lines: 5-6
    - State change: hash-agg concurrency knobs set to `1`
    - Problem: no restore by EOF.
    - Impact: similar to `t/explain.test` and `t/index_join.test`; later cases inherit a tuned execution environment.

22. `t/explain_easy.test`
    - Lines: 10-14, 52, 163
    - State change: several planner/executor session knobs are set explicitly:
      - `tidb_opt_agg_push_down`
      - `tidb_opt_insubq_to_join_and_agg`
      - `tidb_hashagg_partial_concurrency`
      - `tidb_hashagg_final_concurrency`
      - `tidb_window_concurrency`
    - Problem: the file cleans `mysql.opt_rule_blacklist` correctly, but these session knobs are not restored.
    - Impact: good example of a file that cleans one kind of state carefully while still leaving another kind of state behind.

23. `t/explain_easy_stats.test`
    - Lines: 11-14, 44, 65
    - State change: same class of explicit planner/executor knob tuning as `t/explain_easy.test`
    - Problem: no final restore for those session knobs.
    - Impact: later appended explain cases may inherit a non-default planning environment.

24. `t/window_function.test`
    - Lines: 3-4, 13
    - State change:
      - `set @@tidb_enable_window_function = 1`
      - `set @@session.tidb_window_concurrency = 1`
      - later `set @@session.tidb_window_concurrency = 4`
    - Problem: no restore by EOF.
    - Impact: session-level execution environment is left altered for any future cases added to the file.

## Not recorded on purpose

- I did not record cases where the `SET` itself is expected to fail, because failed statements do not leave cleanup debt.
  - Example: invalid `set global collation_* = 'utf8_roman_ci'` in `t/executor/charset.test`
  - Example: the `-- error 8246` `set @@global.tidb_enable_ddl=false` checks in `t/ddl/db_integration.test`
- I checked blacklist / gate-like patterns too.
  - `t/explain_easy.test` updates `mysql.opt_rule_blacklist`, but it also deletes the inserted row and reloads the blacklist, so I did not record it as a finding.
  - The closest real leftover in that family is `tidb_opt_fix_control`, recorded above in `t/planner/core/integration.test`.
- I also skipped many planner/session tuning knobs that are set without an explicit `default` restore when the leftover value was not clearly non-default from the file alone.
  - Those are real hygiene candidates too, but the confidence/signal is lower than the items listed above.
- I no longer treat `t/sessiontxn/externals.test` as a normal cleanup candidate.
  - `tidb_external_ts` is monotonic in the mock store used by these tests and cannot be decreased back to `default` / `0` once increased.
  - That makes a simple tail `set global tidb_external_ts = default` invalid, so this file would need a different isolation strategy if we ever want to clean it up.

## Suggested follow-up order

1. Fix the global-variable leftover first:
   - `t/session/temporary_table.test`
2. Then fix the session `sql_mode` tail leftovers, starting with the files most likely to grow further:
   - `t/expression/builtin.test`
   - `t/expression/time.test`
   - `t/ddl/column_type_change.test`
3. Then fix the non-`sql_mode` environment leftovers that can silently change later plan/transaction behavior:
   - `t/executor/executor.test`
   - `t/session/txn.test`
   - `t/planner/core/integration.test`
   - `t/planner/core/integration_partition.test`
