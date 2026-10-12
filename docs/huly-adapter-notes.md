# Huly adapter notes (prototype)

The `huly` tracker adapter connects Sortie to one project in a Huly workspace. It is a prototype on the `prototype/huly-tracker` branch. The architecture specification, the workflow reference, and the changelog do not describe it yet.

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

Each configured state name must be a status in the project's workflow, compared without regard to letter case. If a configured state is missing, the constructor fails and the error lists the project's states. The classic Huly workflow has no review status, and a handoff state must not be terminal. Before you set `handoff_state`, add a status such as `In Review` to the project in Huly.

## Plan task types

Plan writes map the planning kinds `epic` and `story` to two task types of the project's space type, named by `huly.epic_type` and `huly.story_type`. The default classic project type has only the `Issue` task type, so create the two types first:

1. Open Settings, then **Spaces**, and select the project's space type under **Space types** (`Classic project` for a classic project).
2. In **Task types**, select **+**. Name the type `Epic`, keep the kind **Task & Sub-task**, and create it.
3. Repeat for `Story`.

A change to the space type applies to every project of that type. The adapter resolves both names when it starts, compared without regard to letter case. If one is missing, the constructor fails and the error lists the project's task types. With neither key set, the adapter still reads and writes as before, and a plan write fails with `tracker_payload_error`. The same task types can be created through the transactor by replaying what the settings page sends: push the type's statuses into the space type's `statuses`, create a mixin class that extends `tracker:class:Issue`, create the `task:class:TaskType` document, and push its id into the space type's `tasks`.

## Plan keys

Plan writes store each issue's plan key in a custom attribute, `sortiePlanKey`, on `tracker:class:Issue`. A second, hidden attribute, `sortiePlanBodyDigest`, holds the SHA-256 of the Markdown body the last plan write stored, so a sync can tell whether a body changed without reading it back. The adapter creates both on its first plan write, the way Huly's settings page creates a class attribute: a `TxCreateDoc` of `core:class:Attribute` in `core:space:Model`, marked custom and read-only. Before that it reads the model, so a second adapter, or a restart, creates nothing. On v0.7.426 this needs no workspace upgrade, and the issue page shows the key as **Sortie plan key**.

The adapter finds an issue by key with `find-all` on the attribute, matched by `$in`. The key is separate from the title, status, and labels, so a rename does not affect it: a live check stored a key, renamed the issue in Huly's UI, and found it again by key.

To remove the attributes, delete them in Settings, **Classes**, **Issue**, or send a `TxRemoveDoc` for each `core:class:Attribute` document named `sortiePlanKey` or `sortiePlanBodyDigest`. Issues keep the stored values in their data, and Huly no longer shows them.

## How it works

- At startup, the adapter reads `/config.json` from the front. It sends the token to the account service's `selectWorkspace` method and gets a workspace token and the transactor URL. It also reads the caller's social id, which it writes as `modifiedBy` on its transactions.
- Reads use `POST /api/v1/find-all/<workspace>` on the transactor. Writes use `POST /api/v1/tx/<workspace>` and send `TxCreateDoc`, `TxUpdateDoc`, or `TxRemoveDoc` documents.
- Huly omits a field that a query matches by equality from the documents it returns. The adapter therefore matches by `$in` whenever it reads a queried field back.
- The collaborator service stores descriptions as ProseMirror documents. The adapter reads them with the `getContent` RPC. The `markup` package converts them to Markdown and also converts Huly's own nodes: mentions, emoji, todo lists, and mermaid diagrams. The renderer comes from Probo's MIT-licensed `pkg/prosemirror`; see `internal/tracker/huly/markup/LICENSE.probo`.
- A comment is a `chunter:class:ChatMessage` in the issue's `comments` collection. A label is a `tags:class:TagReference` in the `labels` collection. When no issue label of that name exists, the adapter creates one in the `Other` category.
- Blockers come from the issue's `blockedBy` list, so candidates carry them.

## Test

```sh
SORTIE_HULY_TEST=1 SORTIE_HULY_URL=http://localhost:8087 SORTIE_HULY_TOKEN=... \
SORTIE_HULY_WORKSPACE=sortie SORTIE_HULY_PROJECT=HULY SORTIE_HULY_ISSUE=HULY-5 \
  go test ./internal/tracker/huly/ -run Integration -v
```

The test posts a comment on `SORTIE_HULY_ISSUE`. It also moves the issue to another state and back, and adds and removes a label.

## Known gaps

- Candidate reads do not paginate. One `find-all` call returns every matching issue.
- Candidate issues have no description. Only `fetch_issue_by_id` reads descriptions, one collaborator call per issue.
- The adapter gets the workspace token once, at startup. It does not log in again if the token expires.
- The adapter ignores `tracker.query_filter`.

## Gotcha: a wrong workspace name

When the requested name matches no workspace, Huly's `selectWorkspace` does not fail. It selects some other workspace of the account. The adapter compares the selected workspace with `huly.workspace` and refuses to start when they differ.
