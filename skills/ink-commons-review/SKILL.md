---
name: ink-commons-review
description: Review an exact GitHub PR revision assigned through Ink Commons; retained for existing reviewer skill installations.
---

# Review an Ink Commons task

New installations should use the complete `ink-commons-workflow` skill installed by `ink init`. This reviewer entry remains for existing installations.

For a cooperative task wake, first run `ink task claim --task TASK_ID`. Only a successful `first:true` authorizes work. On `first:false` or conflict, report the state and end the turn rather than reviewing again.

Read the returned delivery for the exact SHA, assigned GitHub identity/command and context cutoff. Read enabled background via `ink context`. Inspect that revision and run appropriate checks in an isolated worktree when necessary. Submit a formal APPROVED or CHANGES_REQUESTED GitHub review using the assigned identity and explicit `commit_id`; a conversation reply is not a formal review.

After all task tools and writes have finished, submit `ink task submit --task TASK_ID --handoff --request-id STABLE_ID --review-id REVIEW_ID --outcome APPROVED` (or CHANGES_REQUESTED). If blocked, submit `--outcome blocked --reason REASON --handoff`. Retry an ambiguous submission only with the same request ID and evidence. Do no further task work after handoff. Report the review link and end the turn; pending verification belongs to Ink. Do not sleep, loop, start background polling or claim successor tasks to wait for review/CI.

Legacy deliveries without a cooperative task ID do not support claim/submit; submit the assigned formal review, report its link and end the turn. Do not invent a task ID or claim completion of execution termination.
