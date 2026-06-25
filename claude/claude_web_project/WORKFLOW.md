# Workflow Guide

This document explains how all project files work together, when each file is updated, and who is responsible for each update. Read this once. Refer back when confused about which file to touch.

---

## The Core Principle

**Claude generates content. You apply it.**

Claude never directly modifies your files — it generates the updated content and you paste/replace. This means you always review what changes before they land. Nothing is ever silently updated.

---

## File Ownership Map

| File | Updated By | When | How |
|------|-----------|------|-----|
| `CLAUDE.md` | Claude generates, you apply | End of every session | Replace entire file with Claude's output |
| `PHASE_N.md` (current) | Claude generates checkboxes, you apply | During and after each session | Check off completed items, paste new content |
| `ARCHITECTURE.md` | Claude generates, you apply | When structure changes | Replace changed sections |
| `DECISIONS_LOG.md` | Claude generates, you apply | When a new decision is made | Append new ADR to bottom of file |
| `ROADMAP.md` | You | When a phase is completed | Update phase status manually |
| `CODING_GUIDELINES.md` | Claude generates, you apply | When new patterns are established | Append new rules |
| `README.md` | You | When setup steps change | Update manually |
| `PROMPTS.md` | Neither | Reference only | Do not modify during development |

---

## File-by-File Usage

---

### CLAUDE.md
**Purpose:** Working memory for every AI session. The most important file in the project.

**Read by Claude:** At the start of every session, before anything else.

**Updated:** At the end of every session. Claude generates the complete new content. You replace the old file entirely. Do not selectively merge — replace the whole thing.

**What triggers an update:**
- Any checklist item is completed
- A new architectural decision is made
- Technical debt is introduced
- The next recommended task changes
- A session ends

**What happens if you skip updating it:**
The next session starts with stale context. Claude will confidently describe the wrong current state, recommend work that is already done, or miss technical debt that was just introduced. This is worse than no context.

**Rule:** Never start a new session without a fresh CLAUDE.md from the previous session end.

---

### PHASE_N.md (current phase file)
**Purpose:** The authoritative specification and checklist for the phase currently being built.

**Read by Claude:** At the start of every session alongside CLAUDE.md.

**Updated:** When checklist items are completed. Claude marks them with ✅ in the session summary. You apply the marks to the actual file.

**What triggers an update:**
- A checklist item is completed
- A design decision within the phase changes (e.g., an API endpoint changes shape)
- A new risk is discovered
- Technical debt specific to this phase is identified

**At phase completion:** The phase file gets a final status update from 🔄 to ✅ COMPLETE with the completion date. It then becomes a historical reference only.

**Do not modify:** The acceptance criteria section. If acceptance criteria need to change, that is a significant decision that requires discussion and a new ADR entry.

---

### ARCHITECTURE.md
**Purpose:** The single source of truth for how the system is built right now. Reflects implemented state, not plans.

**Read by Claude:** When making decisions that affect system structure.

**Updated:** When the structure of the system changes. Not when code is added within an existing structure — only when the structure itself changes: new layers, new packages, changed contracts between components, updated DB schema, new state machine transitions.

**What triggers an update:**
- A new package is added with a defined responsibility
- A component interface changes
- The DB schema changes
- The state machine gains a new state or transition
- The EventBus is swapped (Phase 2)
- A new service is extracted (Phase 3+)

**Rule:** Architecture changes and ARCHITECTURE.md updates happen in the same session. Never let the code drift from the architecture document.

---

### DECISIONS_LOG.md
**Purpose:** Append-only record of every architectural decision with the alternatives that were rejected.

**Read by Claude:** When a new decision needs to be made (to check for prior related decisions) and when explaining past decisions.

**Updated:** Only by appending. Never edit existing ADRs. If a decision is reversed, write a new ADR that supersedes the old one and references it.

**What triggers an update:**
- Any decision is made that affects system architecture, technology choice, or design pattern
- A previously rejected option is now being adopted (write a new ADR, reference the old one)
- A Phase 1 decision needs to change for Phase 2

**Format:** Always use the ADR format defined in the file. ADR numbers are sequential and never reused.

**Value of this file:** When an interviewer asks "why did you use X instead of Y?" the answer is in this file with the exact reasoning. Do not let this file become a rubber-stamp. Every ADR must include genuine alternatives that were actually considered.

---

### ROADMAP.md
**Purpose:** High-level phase plan and learning curriculum.

**Updated by:** You, manually.

**What triggers an update:**
- A phase is completed (update status from 🔄 to ✅)
- A phase is started (update status from ⬜ to 🔄)
- A significant scope decision is made about a future phase

**What does NOT trigger an update:**
- Individual checklist items within a phase (that is the PHASE_N.md file)
- Implementation details (that is ARCHITECTURE.md)

---

### CODING_GUIDELINES.md
**Purpose:** Rules that keep code consistent across all phases and sessions.

**Updated:** Rarely. Only when a genuinely new pattern is established that should be standard going forward.

**What triggers an update:**
- You discover a pattern that should be used everywhere (Claude proposes, you approve, append)
- An existing rule proves unworkable and needs modification (note the superseded rule, add the new one)
- A new library or tool establishes new conventions

**What does NOT trigger an update:**
- Every session. This file is stable.
- Personal preferences. Rules must have a justifiable reason.

---

### README.md
**Purpose:** Getting a developer (future you) from zero to running in under 10 minutes.

**Updated by:** You, manually, when setup steps change.

**What triggers an update:**
- A new dependency is added that requires installation
- A new Makefile target is added
- The environment variables change
- The project structure changes significantly

---

## Session Workflow

Every development session follows this exact sequence.

### Starting a Session

1. Open a new chat in Claude Projects
2. Paste the **New Session Start Prompt** from `PROMPTS.md`
3. Wait for Claude to confirm the current state
4. Confirm or redirect the next task
5. Begin implementation

### During a Session

When Claude makes a new architectural decision mid-session:
- Ask Claude to write the ADR entry immediately
- Do not wait until end of session — decisions made mid-session get forgotten

When a checklist item is completed:
- Note it in the conversation ("step 3 of the checklist is done")
- Claude will track this in the session summary

### Ending a Session

1. Paste the **End of Session Prompt** from `PROMPTS.md`
2. Claude generates: session summary + complete new CLAUDE.md content
3. You replace `CLAUDE.md` in your project files with the new content
4. If any ADRs were generated: append them to `DECISIONS_LOG.md`
5. If architecture changed: apply updates to `ARCHITECTURE.md`
6. If checklist items completed: update `PHASE_N.md` checkboxes
7. Attach updated files back to the Claude Project

### Starting a New Session After Completion

If the previous session ended mid-task:
- Use **New Session Start Prompt** — CLAUDE.md has the context

If the previous session completed a major milestone (e.g., one full step of the checklist):
- Use **Continue From Last Session Prompt** from `PROMPTS.md`
- Paste the session summary from the previous session into the prompt

---

## File Attachment Strategy for Claude Projects

Not all files need to be read every session. Claude Projects allows you to have files in the knowledge base that are referenced when relevant.

**Always attach (high-priority context):**
- `CLAUDE.md` — read every session
- `PHASE_N.md` (current phase only) — read every session

**Attach as reference (Claude reads when relevant):**
- `ARCHITECTURE.md` — referenced for structural decisions
- `CODING_GUIDELINES.md` — referenced when writing code
- `DECISIONS_LOG.md` — referenced when making new decisions

**Attach once, rarely referenced:**
- `ROADMAP.md` — context for phase boundaries
- `README.md` — setup reference

**Do not attach:**
- Completed phase files (PHASE_1.md when you are in Phase 2, etc.) — they add noise

**Recommendation:** When moving to a new phase, remove the previous phase's MD file from the project and add the new one. Completed phase files can be kept in your git repository as historical reference.

---

## What Breaks the System

**Skipping CLAUDE.md update at session end.**
This is the single most common failure mode. The next session starts with wrong context. Claude recommends already-done work. Technical debt is forgotten. Decisions are re-made. One skipped update compounds into significant confusion.

**Updating ARCHITECTURE.md weeks after the code changed.**
The document becomes aspirational fiction. Future sessions reason from the wrong architecture. Always update in the same session as the change.

**Adding features outside the current phase checklist without updating PHASE_N.md.**
Scope creep disguised as productivity. The checklist loses meaning. Acceptance criteria become unclear.

**Making decisions verbally in chat without logging the ADR.**
Six months from now you will not remember why you made that choice. Interviewers will ask. The log is useless if it only captures easy decisions.
