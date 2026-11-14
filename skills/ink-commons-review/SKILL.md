---
name: ink-commons-review
description: Review an Ink Commons task when given a GitHub pull request URL and the exact commit to review.
---

# Review an Ink Commons task

1. Read the PR, acceptance criteria and repository instructions. Confirm the specified commit and inspect the changes in that revision.
2. Run the checks needed for the review in a separate worktree if local execution is necessary. If the commit is unavailable or access is blocked, explain the blocker in the task conversation.
3. Submit a formal GitHub review using the configured reviewer identity and the task's commit SHA. The GitHub create-review API accepts the SHA as `commit_id`. Use Approve when the change passes and Request changes for actionable findings. Include the reason, affected location and verification evidence.
4. Report the submitted review link in the task conversation. If the PR head changed, report which commit was reviewed. Ink Commons tracks the latest revision independently.
