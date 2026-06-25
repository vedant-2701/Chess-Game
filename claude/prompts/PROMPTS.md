# Session Prompts

Copy-paste prompts for every session type. Do not improvise these. The prompts are designed to enforce the project operating rules consistently.

---

## PROMPT 1 — New Session Start

Use this at the beginning of every new chat session.

```
I am continuing development on the chess-server project in Go.

You are acting as a Principal Engineer, Staff System Designer, and Technical Reviewer.
Your job is NOT to agree with me. Optimize for technical correctness, not for making me feel good.

Before doing anything else, read the attached project files in this exact order:
1. CLAUDE.md — current phase, checklist status, architectural constraints, next task
2. PHASE_1.md (or current phase file) — full spec, API contract, acceptance criteria
3. ARCHITECTURE.md — system structure, component responsibilities, data flow

After reading, respond with:
1. Current phase and status (one line)
2. What was completed in the last session (bullet points from CLAUDE.md session log)
3. Known technical debt (from CLAUDE.md)
4. The next recommended task (exact step from the checklist)
5. Any concerns or issues you noticed in the current state

Then ask: "Do you want to proceed with [next task] or work on something else?"

Do NOT write any code until I confirm what we are working on.
Do NOT skip the file review. If the files are not attached, tell me before proceeding.
```

---

## PROMPT 2 — End of Session

Use this when you are done working for the session and want to capture progress.

```
This session is ending. Generate the full session output in two parts.

PART 1 — SESSION SUMMARY

Format exactly as follows:

## Session Summary — [describe what was worked on in 5 words]

### What Was Built
[Specific, concrete. Not "worked on the store layer." Instead: "Implemented GameStore.CreateGame, GetGame, UpdateCurrentFEN with pgx/v5. All three methods have integration tests passing with -race flag."]

### Decisions Made
[List any new architectural decisions made this session. For each: what was decided and why. If a full ADR should be written, flag it.]

### Tradeoffs Considered
[What alternatives were discussed and why they were not chosen]

### Lessons Learned
[What did the implementation reveal that was not obvious from the design? What would you do differently?]

### Problems Encountered
[What broke, what was harder than expected, what is still unresolved]

### Checklist Progress
[List every checklist item touched this session with status: ✅ Complete, 🔄 In Progress, ❌ Blocked]

### Technical Debt Introduced
[Any shortcuts taken. Format: TD-00X: description | Phase introduced: N | Must fix by: Phase Y]

### Files Modified
[List every file created or modified]

### Recommended Next Step
[One specific task. Not "continue the store layer." Instead: "Implement auth/token.go: SignPlayerToken and VerifyPlayerToken with unit tests. Estimated 1-2 hours."]

---

PART 2 — UPDATED CLAUDE.md

Generate the complete, updated CLAUDE.md content. This is a full replacement of the current CLAUDE.md, not a diff. Every section must be updated to reflect the current state after this session. The "Next Recommended Task" section must reflect the output of PART 1. The session log must have a new entry for this session.

I will copy this output and replace my CLAUDE.md file with it.
```

---

## PROMPT 3 — Continue From Last Session (New Topic)

Use this when starting a new chat after a major milestone was completed (e.g., one full step of the checklist is done and you are starting the next step). Paste the Part 1 session summary from the previous session into this prompt.

```
I am starting a new chat to continue the chess-server project. The previous session just completed a milestone.

Previous session summary:
---
[PASTE PART 1 SESSION SUMMARY FROM PREVIOUS SESSION HERE]
---

The project files attached to this project (CLAUDE.md, PHASE_1.md, ARCHITECTURE.md) reflect the updated state after that session.

You are acting as a Principal Engineer, Staff System Designer, and Technical Reviewer.
Your job is NOT to agree with me. Optimize for technical correctness.

Do the following:
1. Read CLAUDE.md to confirm current state matches the session summary above
2. Flag any inconsistencies between the session summary and CLAUDE.md
3. Confirm the next recommended task
4. Review whether the completed work introduced any risks or open questions that should be addressed before moving forward
5. Ask: "Do you want to proceed with [next task] or is there anything from last session to revisit?"

Do NOT write any code until I confirm.
```

---

## PROMPT 4 — Mid-Session Architecture Review

Use this when you have written a significant chunk of code and want a senior review before continuing.

```
I have implemented [describe what was built]. Before continuing, I need a senior engineer review.

Review the following code/design with zero tolerance for:
- Wrong assumptions
- Overengineering
- Missing error handling
- Concurrency issues (race conditions, deadlocks, goroutine leaks)
- Violations of CODING_GUIDELINES.md
- Deviations from ARCHITECTURE.md

For every issue found:
1. State what is wrong
2. State why it is wrong
3. State what should be done instead

Do not praise the code unless you can justify it with specific reasoning.
Do not soften criticism.

[PASTE CODE HERE]
```

---

## PROMPT 5 — New ADR Decision

Use this when you are facing an architectural decision mid-session and want to log it properly.

```
I need to make an architectural decision and want to log it as an ADR.

Context: [describe the situation forcing the decision]

Options I am considering:
- Option A: [describe]
- Option B: [describe]
- Option C (if any): [describe]

My current lean: [which option and why]

Do the following:
1. Critically evaluate each option — do NOT default to agreeing with my lean
2. Identify any options I have not considered
3. State the recommended option with specific technical justification
4. Generate the full ADR entry in the format used in DECISIONS_LOG.md, ready to append

The ADR number should be the next sequential number after the last entry in DECISIONS_LOG.md.
```

---

## PROMPT 6 — Phase Completion Review

Use this when all checklist items in a phase are marked complete and before starting the next phase.

```
Phase [N] checklist is complete. Before marking the phase done and starting Phase [N+1], conduct a phase completion review.

Review against PHASE_N.md acceptance criteria:
1. Go through each acceptance criterion one by one
2. For each criterion: state whether it is MET or NOT MET
3. For NOT MET criteria: describe exactly what is missing
4. Identify any technical debt that was introduced but not logged in CLAUDE.md
5. Identify any ARCHITECTURE.md sections that are out of date
6. Identify any DECISIONS_LOG.md entries that should have been written but were not

Then:
- If all criteria are met: generate the phase completion note (status update for CLAUDE.md and ROADMAP.md)
- If any criteria are not met: list exactly what must be done before the phase is complete

Do NOT declare the phase complete if any acceptance criterion is not met. Be strict.
```

---

## PROMPT 7 — Debugging a Specific Problem

Use this when stuck on a bug or unexpected behavior.

```
I have a problem I cannot resolve. Before suggesting solutions, ask me clarifying questions if needed.

Problem: [describe the exact symptom]
Expected behavior: [what should happen]
Actual behavior: [what is happening]
What I have already tried: [list attempts]

Relevant code:
[PASTE CODE]

Relevant logs or error output:
[PASTE OUTPUT]

Constraints:
- Do not suggest changing the architecture unless the bug reveals a fundamental design flaw
- Do not suggest solutions that violate CODING_GUIDELINES.md
- If this reveals a design flaw worth logging as technical debt, flag it

Diagnose the root cause first. Do not jump to solutions.
```

---

## PROMPT 8 — Pre-Phase Planning

Use this before starting a new phase (after Phase 1, before Phase 2, etc.).

```
I am about to start Phase [N] of the chess-server project.

Before writing any code, I need a pre-phase architecture review.

Read:
1. PHASE_N.md for the phase I am about to start
2. ARCHITECTURE.md for current system state
3. CLAUDE.md for current technical debt

Then:
1. Identify any technical debt from previous phases that MUST be resolved before Phase N can proceed
2. Identify any Phase 1 design decisions that will break under Phase N requirements
3. Confirm the EventBus interface seam is correctly positioned for Phase 2 (or equivalent for other phases)
4. Propose any ARCHITECTURE.md updates needed before the first line of Phase N code is written
5. Flag anything in PHASE_N.md that seems underspecified or will cause problems

Be direct. If Phase N cannot start cleanly, say so and list what must be fixed first.
```

---

## Quick Reference

| Situation | Use Prompt |
|-----------|-----------|
| Starting any new session | Prompt 1 |
| Ending any session | Prompt 2 |
| New chat after completing a milestone | Prompt 3 |
| Want code reviewed mid-session | Prompt 4 |
| Facing an architectural decision | Prompt 5 |
| All checklist items done, phase ending | Prompt 6 |
| Stuck on a bug | Prompt 7 |
| About to start a new phase | Prompt 8 |
