---
tracker:
  kind: huly
  endpoint: $SORTIE_HULY_URL
  api_key: $SORTIE_HULY_TOKEN
  project: $SORTIE_HULY_PROJECT
  active_states:
    - Todo
    - In Progress
  terminal_states:
    - Done
    - Canceled
  in_progress_state: In Progress
  # Huly's classic workflow has no review state, and the handoff state may
  # not be terminal. Add a status such as "In Review" to the project's
  # workflow in Huly, then uncomment:
  # handoff_state: In Review

huly:
  workspace: $SORTIE_HULY_WORKSPACE

polling:
  interval_ms: 30000

workspace:
  root: /tmp/sortie-huly-workspaces

agent:
  kind: mock
  max_turns: 3
  max_concurrent_agents: 2
---

{{/* Huly prototype workflow with the mock agent: dispatches every Todo or
     In Progress issue of the project, moves it to In Progress, and runs
     the mock agent. Swap the agent block for a real agent once the tracker
     side behaves.
     Required env vars:
       SORTIE_HULY_URL        Huly front URL, e.g. http://localhost:8087
       SORTIE_HULY_TOKEN      Huly account token (see docs/huly-adapter-notes.md)
       SORTIE_HULY_PROJECT    Project identifier, e.g. HULY
       SORTIE_HULY_WORKSPACE  Workspace URL name, e.g. sortie */}}

**{{ .issue.identifier }}**: {{ .issue.title }}

{{ if .issue.description }}{{ .issue.description }}{{ end }}

{{ if .run.is_continuation }}Continue working on the task. Review current state and proceed.{{ end }}
