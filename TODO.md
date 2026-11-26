# Roadmap

Ink Commons v0.1 focuses on a reliable, protocol-neutral GitHub PR orchestration core on Linux and macOS. Planned items for v0.2 and beyond:

## v0.2+ Candidates

- [ ] **GitHub Webhook ingestion**: Optional webhook receiver to complement polling, reduce review/rework latency, and further conserve GitHub API quota.
- [ ] **Native `github_auto_merge` merge strategy**: Implement a GitHub native auto-merge strategy behind the existing `MergeStrategy` interface alongside the v0.1 SHA-locked `direct` strategy.
- [ ] **Structured / localized runtime alert payloads**: Decouple alert rendering from hardcoded alert text by standardizing structured alert codes and metadata across API responses.
- [ ] **Public Go protocol / ledger package evaluation**: Re-evaluate exporting stable protocol or ledger packages once Adapter HTTP v1 settles in production.
- [ ] **Managed executor adapters**: Reference adapter implementations supporting durable runtime termination, delayed-delivery fencing, and `managed: true` admission.
- [ ] **Forge abstraction**: Evaluate decoupling GitHub-specific observation and merge interfaces into a generic forge boundary.
