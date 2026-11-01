# Huly adapter notes (prototype)

The `huly` tracker adapter connects Sortie to one project in a Huly workspace. It is a prototype on the `prototype/huly-tracker` branch and the plan sync work built on it. The workflow reference describes its configuration keys, and the changelog its plan writes; the architecture specification lists it only as the adapter that implements the write capabilities (§11.7).

## Run Huly locally

1. Clone `https://github.com/hcengineering/huly-selfhost` next to this repository.
2. Run `./setup.sh --quick`. The script writes `huly_v7.conf` and starts the stack with `docker compose`.
3. Open `http://localhost:8087`.

Podman notes:

- Podman rejects the empty `HTTP_BIND` value in `huly_v7.conf`. Set `HTTP_BIND=127.0.0.1`, then run `docker compose up -d nginx`.
- Podman needs fully qualified image names. For the compose run, set `CONTAINERS_REGISTRIES_CONF` to a file that contains `unqualified-search-registries = ["docker.io"]`.
- If Docker Hub rate-limits the Redpanda image, pull `mirror.gcr.io/redpandadata/redpanda:<tag>`. Then tag it as `docker.redpanda.com/redpandadata/redpanda:<tag>`.

## Get a token

Huly v0.7.426, the current self-host release, has no API tokens. Use a login token instead. A login token has no expiry, so treat it like a password.

Use this request to create an account:

```sh
curl -s -X POST http://localhost:8087/_accounts -H 'Content-Type: application/json' \
  -d '{"method":"signUp","params":{"email":"you@example.com","password":"...","firstName":"Sortie"}}'
```

If the account exists already, use the `login` method with `email` and `password`. The token is in `result.token`. With that token, the `createWorkspace` method (`{"workspaceName":"..."}`) creates a workspace.

Newer Huly versions also provide revocable API tokens through `createApiToken`. The adapter accepts both kinds of token.

## Configure

```yaml
tracker:
  kind: huly
  endpoint: http://localhost:8087   # the Huly front URL
  api_key: $SORTIE_HULY_TOKEN
  project: HULY                     # the prefix of issue keys such as HULY-12
  active_states: [Todo, In Progress]
  terminal_states: [Done, Canceled]

huly:
  workspace: sortie                 # the workspace URL name
  epic_type: Epic                   # task types for plan writes; set both or neither
  story_type: Story
```

`examples/WORKFLOW.huly.md` is a complete workflow. It uses the mock agent.

Each configured state name must be a status in the project's workflow, compared without regard to letter case. That covers `active_states`, `terminal_states`, `handoff_state`, `in_progress_state`, `no_change_state`, and the plan-control states `release_state`, `backlog_state`, `cancel_state`, and `done_state`. If a configured state is missing, the constructor fails and the error lists the project's states. The classic Huly workflow has no review status, and a handoff state must not be terminal. Before you set `handoff_state`, add a status such as `In Review` to the project in Huly.

## Plan task types

Plan writes map the planning kinds `epic` and `story` to two task types of the project's space type, named by `huly.epic_type` and `huly.story_type`. The default classic project type has only the `Issue` task type, so create the two types first:

1. Open Settings, then **Spaces**, and select the project's space type under **Space types** (`Classic project` for a classic project).
2. In **Task types**, select **+**. Name the type `Epic`, keep the kind **Task & Sub-task**, and create it.
3. Repeat for `Story`.

A change to the space type applies to every project of that type. The adapter resolves both names when it starts, compared without regard to letter case. If one is missing, the constructor fails and the error lists the project's task types. With neither key set, the adapter still reads and writes as before, and a plan write fails with `tracker_payload_error`. The same task types can be created through the transactor by replaying what the settings page sends: push the type's statuses into the space type's `statuses`, create a mixin class that extends `tracker:class:Issue`, create the `task:class:TaskType` document, and push its id into the space type's `tasks`.

## Plan projects

The adapter implements `PlanProjectEnsurer`, so the plan routes serve every project of the workspace through `?project=<key>`, and `GET` and `PUT /api/v1/plan/projects/<key>` inspect and create projects.

- A key is the project identifier: a letter, then up to four letters or digits, upper case.
- Creating sends a `TxCreateDoc` of `tracker:class:Project` in `core:space:Space`. The new project is public, has the caller's account as member and owner, and takes its space type and default issue status from `tracker.project`, so it has the same task types and workflow.
- Inspection reports a project as not ready when its space type lacks a task type named by `huly.epic_type` or `huly.story_type`, or a state the workflow names. The message names the space type and how many projects share it. The adapter never changes a space type: add what is missing in Settings, as in "Plan task types". A change there applies to every project of that type.
- Each other project gets its own adapter, and its own login, the first time a plan route names it.

## Kind mapping

| Plan kind | Huly | Parent |
|---|---|---|
| `epic` | An issue of task type `huly.epic_type` | None (`tracker:ids:NoParent`) |
| `story` | An issue of task type `huly.story_type` | Its epic's issue, through `attachedTo` and `parents`; Huly shows it as a sub-issue |

A blocking relation is an entry `{_id, _class: "tracker:class:Issue"}` in the blocked issue's `blockedBy` list, the same list candidate reads take blockers from. The adapter declares every write capability, `create_issue`, `update_issue`, `set_parent`, and `link_dependency`, with `custom_field` plan-key storage. An issue of any other task type reads back with no kind.

## Plan keys

Plan writes store each issue's plan key in a custom attribute, `sortiePlanKey`, on `tracker:class:Issue`. A second, hidden attribute, `sortiePlanBodyDigest`, holds the SHA-256 of the Markdown body the last plan write stored, so a sync can tell whether a body changed without reading it back. The adapter creates both on its first plan write, the way Huly's settings page creates a class attribute: a `TxCreateDoc` of `core:class:Attribute` in `core:space:Model`, marked custom and read-only. Before that it reads the model, so a second adapter, or a restart, creates nothing. On v0.7.426 this needs no workspace upgrade, and the issue page shows the key as **Sortie plan key**.

Three more read-only attributes hold a story's approver, written by plan sync under plan control: `sortieApprovedBy` (**Approved by**, the display name), `sortieApprovedAt` (**Approved at**, RFC 3339), and the hidden `sortieApprovedById` (the stable identity). They are created the same way on the next plan write, so a workspace that already has the first two gets only these. The adapter declares the `approval_fields` capability for them. A plan story can also be created in a named status (`tracker.backlog_state` under plan control). The status must belong to the story's task type, or the create fails before it takes an issue number.

The adapter finds an issue by key with `find-all` on the attribute, matched by `$in`. The key is separate from the title, status, and labels, so a rename does not affect it: a live check stored a key, renamed the issue in Huly's UI, and found it again by key.

To remove the attributes, delete them in Settings, **Classes**, **Issue**, or send a `TxRemoveDoc` for each `core:class:Attribute` document named `sortiePlanKey`, `sortiePlanBodyDigest`, `sortieApprovedBy`, `sortieApprovedById`, or `sortieApprovedAt`. Issues keep the stored values in their data, and Huly no longer shows them.

## Creating issues

`CreateIssue` follows Huly's importer, because Huly computes issue numbers and order on the client:

1. It sends a `TxUpdateDoc` with `$inc: {sequence: 1}` on the project, in space `core:space:Space`, with `retrieve: true`. The transactor applies the increment atomically and answers with the updated project, whose `sequence` is the new issue number. The identifier is `<project>-<number>`.
2. It ranks the issue after the project's highest rank, with a port of `@hcengineering/rank` (`rank.go`, tested against that package's own cases).
3. It converts the Markdown body to markup and stores it with the collaborator's `createContent` RPC, which returns the blob reference for `description`.
4. It sends a `TxCreateDoc` of `tracker:class:Issue` in the parent's `subIssues` collection, or under `tracker:ids:NoParent`, with the task type in `kind` and the plan key and body digest in their attributes. The transactor increments the parent's `subIssues` count itself.

A new issue gets the project's default issue status when its task type allows it, and the type's first status otherwise (`Backlog` on a classic project). A live check created an epic and a story under it; Huly showed the story as a sub-issue of the epic, with task types Epic and Story and both descriptions rendered.

## Updating, parenting, and linking

- `UpdateIssue` writes only fields that differ. A new body goes through the collaborator's `updateContent` when the issue has a description, because `createContent` refuses a document that is open in an editor; the collaborator stores it and updates `description` itself. An issue without a description gets one through `createContent`. Either way the body digest is updated.
- `SetParent` updates `attachedTo` and the `parents` chain in one `TxUpdateDoc`, filed in the old parent's `subIssues` collection as Huly's parent picker does, so the transactor moves the sub-issue counts. It refuses to make an issue its own ancestor.
- `AddDependency` and `RemoveDependency` read the blocked issue first and send `$push` or `$pull` on `blockedBy` only when the relation is missing or present. Linking twice and unlinking a missing relation succeed without a write.
- Every write accepts only issues of the configured project; an issue of another project is `tracker_not_found`.

## How it works

- At startup, the adapter reads `/config.json` from the front. It sends the token to the account service's `selectWorkspace` method and gets a workspace token and the transactor URL. It also reads the caller's social id, which it writes as `modifiedBy` on its transactions.
- Reads use `POST /api/v1/find-all/<workspace>` on the transactor. Writes use `POST /api/v1/tx/<workspace>` and send `TxCreateDoc`, `TxUpdateDoc`, or `TxRemoveDoc` documents.
- Huly omits a field that a query matches by equality from the documents it returns. The adapter therefore matches by `$in` whenever it reads a queried field back.
- The collaborator service stores descriptions as ProseMirror documents. The adapter reads them with the `getContent` RPC. The `markup` package converts them to Markdown and also converts Huly's own nodes: mentions, emoji, todo lists, and mermaid diagrams. The renderer comes from Probo's MIT-licensed `pkg/prosemirror`; see `internal/tracker/huly/markup/LICENSE.probo`.
- A comment is a `chunter:class:ChatMessage` in the issue's `comments` collection. A label is a `tags:class:TagReference` in the `labels` collection. When no issue label of that name exists, the adapter creates one in the `Other` category.
- Blockers come from the issue's `blockedBy` list, so candidates carry them. Each blocker carries its state, plan key, and labels; the labels are read in the same `TagReference` query as the candidates' own, so a blocker's `sortie-repository:<name>` label tells which Sortie runs it.

## Test

```sh
SORTIE_HULY_TEST=1 SORTIE_HULY_URL=http://localhost:8087 SORTIE_HULY_TOKEN=... \
SORTIE_HULY_WORKSPACE=sortie SORTIE_HULY_PROJECT=HULY SORTIE_HULY_ISSUE=HULY-5 \
  go test ./internal/tracker/huly/ -run Integration -v
```

The tests post a comment on `SORTIE_HULY_ISSUE`, move it to another state and back, and add and remove a label. They store a probe plan key on it and clear it again. They create plan issues, an epic with stories, link and unlink them, and remove them at the end; set `SORTIE_HULY_KEEP=1` to keep them for a look in the UI. The plan tests need the task types of "Plan task types"; `SORTIE_HULY_EPIC_TYPE` and `SORTIE_HULY_STORY_TYPE` override the names `Epic` and `Story`.

## Known gaps

- Candidate reads do not paginate. One `find-all` call returns every matching issue.
- Candidate issues have no description. Only `fetch_issue_by_id` reads descriptions, one collaborator call per issue.
- The adapter gets the workspace token once, at startup. It does not log in again if the token expires.
- The adapter ignores `tracker.query_filter`.

## Gotcha: a wrong workspace name

When the requested name matches no workspace, Huly's `selectWorkspace` does not fail. It selects some other workspace of the account. The adapter compares the selected workspace with `huly.workspace` and refuses to start when they differ.

## Live end-to-end check (plan workbench)

On 2026-10-11, the whole planning loop ran against the local Huly v0.7.426, workspace `sortie`, project `HULY`. The parts were Sortie from this branch with the mock agent and a 20-second turn delay, `acp-broker`, and the plan workbench from a clean checkout, following its `docs/operator-guide.md`. The planning agent was OpenCode over the broker. The spec store was a scratch clone of `agent-platform-specs`, so the real store stayed untouched. Before the run, the demo issue `HULY-1` was moved out of `In Progress` so that Sortie would not run the mock agent on it; it was moved back afterwards.

| Step | What happened | Result |
|---|---|---|
| 1. The agent drafts a breakdown | In the chat: "Break the agent-telemetry change into deliverables …". The agent switched the change to `spec-driven-planned`, wrote `breakdown.yaml` with 9 deliverables covering the change's 17 requirements, ran `planlib validate`, found two acceptance criteria that YAML had read as maps, fixed them, and validated again. | The graph redrew with the 9 drafts while the agent wrote. |
| 2. A planted cycle | `session-context` was edited by hand to depend on `turn-recorder`. The validation panel listed `cycle agent-telemetry/session-context -> agent-telemetry/turn-recorder`, the graph kept its last valid state, and Sync was disabled. With `session-context` selected, the chat message "The validator reports a cycle around this deliverable. Fix it …" went out with the selection block. The agent removed the reverse edge and validated. | The panel cleared. |
| 3. Approval | Approve breakdown on the `agent-telemetry` epic. | `status: approved` and `approved_digest` were written; the epic read as approved. |
| 4. Sync preview and confirm | Sync… showed 10 creates, 8 relations to add, and `plan-to-tracker-dag` skipped as a draft. The changes without a breakdown were not approved, so they were not synced. Apply ran 18 writes. | 10 created and 8 relations added; a second preview showed no writes. |
| 5. Huly | `HULY-34` (task type Epic, plan key `agent-telemetry`) with 9 sub-issues `HULY-35`…`HULY-43` of task type Story. Each has its acceptance criteria and covered requirements as its description, and Blocked by / Blocks shows the breakdown's 8 dependencies. | Matches the breakdown. |
| 6. Dispatch | `HULY-36` (`session-context`, unblocked) and `HULY-37` (`turn-recorder`, blocked by `HULY-36`) were moved to `Todo`. | Sortie dispatched only `HULY-36` and moved it to `In Progress`; `HULY-37` stayed in `Todo`. |
| 7. Running badge | The workbench showed `running · turn 1` on `session-context`, `1 agent running` on the `agent-telemetry` epic, and `now · 1 agent` on the playhead, within one five-second poll. | As specified. |

The issues the check created are still in the project as a reference.
