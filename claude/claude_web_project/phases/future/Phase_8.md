## Phase 8 — Container Orchestration

**Status: ⬜ Not Started**
**Prerequisite: Phase 2 all acceptance criteria met**

---

## Objective

Redeploy the Phase 2 fleet onto Kubernetes, replacing the hand-rolled Redis
ownership/liveness directory (`DECISIONS_LOG_PHASE_2.md` ADR-021/ADR-023) with
native platform primitives, and close Phase 2's one deliberately-accepted
residual risk (TD-P2-001) along the way.

**System Design Concept:** The difference between hand-rolling a distributed primitive and adopting a platform that already solved it — and why it was still correct to build Phase 2's version by hand first.

**What Is Built:**
- Redeploy the Phase 2 fleet onto Kubernetes (local cluster — kind or minikube; no cloud provider, consistent with this project's existing infra stance)
- Replace nginx's static Edge Proxy map with a Kubernetes `Service` + `ingress-nginx`
- Replace the hand-rolled Redis lease directory with Kubernetes' native `Lease` API object — same TTL/claim concept as Phase 2, now backed by etcd's Raft consensus instead of a single Redis instance
- `StatefulSet` for stable per-instance identity (`server-0`, `server-1`, ...), replacing Phase 2's manually-assigned `INSTANCE_ID`
- Liveness/readiness probes replacing nginx's passive health checks
- `ConfigMap`/`Secret` replacing `.env`-file configuration

**What This Teaches:**
- Hand-rolled lease (Phase 2, Redis, single point of failure) vs. consensus-backed lease (k8s `Lease`/etcd) — same concept, stronger guarantee, and you'll actually understand *why* it's stronger because you built the weaker version first
- Declarative reconciliation vs. imperative deployment
- Why production systems don't hand-roll what the platform already solved — a lesson that only lands if you've felt the hand-rolled version's edges yourself
- **Fencing tokens for free:** Phase 2 explicitly accepted a narrow, TTL-bounded false-positive liveness window as a documented limitation (TD-P2-001, `DECISIONS_LOG_PHASE_2.md` ADR-023) rather than build ownership-epoch tracking by hand. Kubernetes' `Lease` object carries a `resourceVersion` that gives you exactly this fencing guarantee natively — Phase 8 is where TD-P2-001 actually gets closed, not just superseded by different infrastructure. Migrating for its own sake would be a weaker phase; migrating because it closes a debt you already know about and can name is the honest version.

**Explicitly Out of Scope:** service mesh (Istio/Linkerd), multi-cluster/multi-region, autoscaling policy tuning — separate, deep topics, not bundled in.

**Phase 8 is complete when:**
- Phase 2's core guarantee (failover + origin recovery never causes a split game) holds identically on Kubernetes
- `kubectl scale` requires zero manual config edits, unlike Phase 2's static nginx upstream list
- `kubectl delete pod` recovers with the same player-facing behavior as Phase 2's crash scenarios
- TD-P2-001 is closed: a test that reproduces Phase 2's false-positive liveness window
  (documented in `DECISIONS_LOG_PHASE_2.md` ADR-023) demonstrates the k8s `Lease`-based
  design does **not** split a live game under the same conditions

---