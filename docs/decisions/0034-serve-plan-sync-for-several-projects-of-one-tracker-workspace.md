---
status: accepted
date: 2026-10-15
decision-makers: Brian Cappello
---

# Serve Plan Sync for Several Projects of One Tracker Workspace

## Context and Problem Statement

Plan sync makes a tracker match a validated plan graph. It is served by `sortie plan sync` and by three HTTP routes: `POST /api/v1/plan/sync`, `GET /api/v1/plan/graph`, and `GET /api/v1/plan/capabilities`. Until now every one of them wrote to and read from `tracker.project`, the single project the orchestrator polls and dispatches.

The plan workbench, the planning tool that calls these routes, now manages several plans from one server. Each plan is a spec store paired with one tracker project. Under the old routes the workbench needed one Sortie process per plan, each with its own workflow file, database, port, and control token, only to write epics and stories into a different project of the same tracker workspace with the same credentials. The workbench would have had to start, supervise, and route between those processes, and none of them would have dispatched anything a sibling could not.

The workbench also needs to create the tracker project when a person creates a plan, and to tell the person, before the first sync, when a project cannot take plan writes.

ADR 0019 states that a Sortie process serves one workflow file, one database, and one tracker configuration, with no notion of a tenant in its state. This record decides how far plan sync may reach beyond `tracker.project` without contradicting that.

## Decision Drivers

1. **Plan sync is outside the orchestrator's loop.** It changes no orchestrator state and no dispatch. It holds only its own previews and an apply lock, both in memory. Widening it does not touch polling, reconciliation, workspaces, or the database.
2. **A tracker configuration is a workspace and a credential, not a project.** The endpoint, the token, and the workspace name are what Sortie must keep to one. The project is a filter applied to reads and the destination of writes inside that workspace. Two projects of one workspace share every secret and every network destination.
3. **One process per project is operational cost with no isolation gained.** N processes of one credential against one workspace protect nothing from each other, and they make the caller a process supervisor.
4. **Dispatch is a different, larger question.** Dispatching agents for several projects means per-project states, workspaces, hooks, prompts, concurrency, and budgets, and a project column throughout the database. That should be decided on its own, not slipped in with plan sync.
5. **Creating projects is a write to the workspace, not to a project.** It must be narrow enough that a planning tool can call it safely, and must never alter what other projects depend on.

## Considered Options

- Keep one project per process, and have the caller run one Sortie per plan
- Serve plan sync for several projects of the configured workspace, keep dispatch on `tracker.project`, and let the plan routes inspect and create projects
- Make Sortie fully multi-project: plan sync and dispatch for every configured project

## Decision Outcome

Chosen option: **serve plan sync for several projects of the configured workspace, with dispatch kept on `tracker.project`**, because it removes the per-plan process the caller would otherwise supervise, while leaving the orchestrator, the database, and the meaning of a Sortie process unchanged.

### What a process still is

A Sortie process still serves one workflow file, one database, and one tracker configuration: one endpoint, one credential, one workspace. It still polls and dispatches exactly one project, `tracker.project`. The other projects plan sync reaches belong to the same workspace and are reached with the same credential; nothing about them is persisted, and the orchestrator never sees them. ADR 0019's first reading, that the binary is not a multi-tenant server, holds: there is still one operator, one credential, and no tenant anywhere in Sortie's state.

### Selecting a project

Every plan route takes `?project=<key>`. Without it, the route serves `tracker.project`, so existing callers are unchanged. The configured project keeps the tracker adapter the orchestrator built. Another project gets an adapter of its own, built on first use from the current workflow with only the project replaced, and kept until the process stops. Each project has its own previews and its own apply lock, so a preview applies only to the project that produced it, and applies to different projects do not wait for each other.

### Inspecting and creating projects

An adapter may implement a new optional capability, `PlanProjectEnsurer`, with two operations: inspect a project by key, and create it when the workspace lacks it. Two routes expose them, `GET` and `PUT /api/v1/plan/projects/<key>`, behind the control token like the other plan routes.

Inspection reports whether the project exists, and lists in operator terms what keeps plan writes from working in it: a task type that `huly.epic_type` or `huly.story_type` names and the project's space type lacks, or a state the workflow names and its workflow lacks. Creation makes a project of the same space type as `tracker.project`, so a new project inherits a type that is already known to work, then inspects it.

Neither operation changes a project that exists, nor anything shared between projects. In Huly, task types and workflow states belong to the space type, which every project of that type shares, and they live in the workspace model. Repairing them from a planning tool would change every project of the type. What creation cannot fix is reported, with the place to fix it, and the operator fixes it once.

Other projects are served only when the configured adapter implements this capability. For any other tracker kind, naming a project other than `tracker.project` answers `projects_unsupported`, and plan sync behaves exactly as before.

### Considered Options in Detail

**One project per process.** This option keeps Sortie unchanged and matches ADR 0019 with no clarification. It is rejected because the cost falls entirely on the caller: a workflow file, a database, a port, and a token per plan, plus supervision of their lifecycles, all to write into projects that the one credential already reaches. The processes isolate nothing from each other, since they share the credential and the workspace.

**Full multi-project Sortie.** This option is the only one that runs agents for every plan. It is deferred, not rejected. It needs per-project tracker states, workspace roots, hooks, prompts, concurrency limits, and budgets; an orchestrator loop over several adapters; a project column in the run, retry, and reaction tables, with a migration; and a workspace sweep that asks each project's tracker about its own directories. Each of those is a design decision of its own, and none is needed for plans to be written and synced. Keeping plan sync and dispatch separate now leaves that decision open.

## Consequences

### Positive

- One Sortie process serves plan sync for every plan of a workspace, so a planning tool needs no process supervision.
- Existing callers are unaffected: without `?project=`, every plan route behaves as before.
- Previews and apply locks are per project, so plans cannot interfere with each other's syncs.
- A person creating a plan learns at once whether its tracker project can take plan writes, and what to fix if not.
- The orchestrator, the database schema, and the dispatch loop are unchanged.

### Negative

- **Plans other than `tracker.project` are never dispatched.** Their stories can be synced and moved to Todo, and nothing runs them. A caller must say so, and the graph response carries `dispatched` so it can.
- **Each other project costs a login and an adapter for the life of the process.** They are not released when a plan is deleted, because Sortie does not know about plans. The cost is one idle adapter per project ever named.
- **Adapters for other projects follow workflow reloads only when built.** Like the configured adapter, an adapter built before a reload keeps the settings it was built with.
- **Space types are never repaired.** On a workspace whose space type lacks the plan task types, every new project reports the same problem until an operator adds them in the tracker's settings.

## Specification Material Requiring Update

1. The tracker integration contract: the optional `PlanProjectEnsurer` capability, and that plan sync serves other projects of the workspace only through an adapter that implements it.
2. The HTTP API reference: `?project=` on the plan routes, the two project routes, the `project` and `dispatched` fields of the graph response, and the `project_not_found` and `projects_unsupported` errors.
3. The workflow reference's HTTP table and plan sync section, and the Huly adapter notes.
4. The goals and non-goals material: the multi-tenant non-goal is unchanged, and this record is an example of a capability that stays inside it.

## Confirmation

The decision is validated when all of the following hold:

1. With no `project` parameter, every plan route answers as it did before this change.
2. A sync naming another project writes only into that project, and a preview from one project is refused when applied to another.
3. The graph of a project other than `tracker.project` reports `dispatched: false` and no runtime state on any node, even when the orchestrator runs an issue with the same id.
4. Ensuring a project that exists sends no write to the tracker.
5. No orchestrator code path, database table, or workspace path refers to any project other than `tracker.project`.
