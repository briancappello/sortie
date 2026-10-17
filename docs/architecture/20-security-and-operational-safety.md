## 15. Security and Operational Safety

### 15.1 Trust Boundary Assumption

Each deployment defines its own trust boundary.

Operational safety requirements:

- Deployments should state clearly whether they are intended for trusted environments, more restrictive environments, or both.
- Deployments should state clearly whether they rely on auto-approved actions, operator approvals, stricter sandboxing, or some combination of those controls.
- Workspace isolation and path validation are important baseline controls, but they are not a substitute for whatever approval and sandbox policy a deployment chooses.

### 15.2 Filesystem Safety Requirements

Mandatory:

- Workspace path must remain under configured workspace root.
- Coding-agent cwd must be the per-issue workspace path for the current run.
- Workspace directory names must use sanitized identifiers.

Recommended additional hardening:

- Run under a dedicated OS user.
- Restrict workspace root permissions.
- Mount workspace root on a dedicated volume if possible.

### 15.3 Secret Handling

- Support `$VAR` indirection in workflow config.
- Validate presence of secrets without printing them.
- Every log record and every runtime-derived text Sortie stores or shows an operator has each secret value it holds replaced by a fixed marker. The values are those Sortie holds under a name that marks a credential (its environment variables and environment file, configuration keys, and the environment entries and headers of the tool servers it hands a runtime), the tracker API key, the credentials in a URL's userinfo, and notification endpoint URLs, which grant access to whoever holds them. A value too short to mask is named in a warning, never shown. Matching is exact, and every bound that cuts such text applies after masking. The one exception is the agent's stop statement, which Sortie reads only up to a byte bound before it can mask anything; there masking also removes a trailing fragment that is a prefix of a held value, so the cut cannot leave the start of one behind. A value Sortie never held, one it holds under a name no convention marks as a credential, or one its producer transforms before printing it, is outside what masking recognizes.

### 15.4 Hook Script Safety

Workspace hooks are arbitrary shell scripts from `WORKFLOW.md`.

Implications:

- Hooks are fully trusted configuration.
- Hooks run inside the workspace directory, through the execution environment: on the host under `local`, in the issue's VM under `smolvm` (Section 15.8). A hook in a VM still runs code from the repository, such as a package install script, but that code then runs in the VM.
- Hook output should be truncated in logs.
- Hook timeouts are required to avoid hanging the orchestrator.

### 15.5 Harness Hardening Guidance

Running coding agents against repositories, issue trackers, and other inputs that may contain sensitive data or externally-controlled content can be dangerous. A permissive deployment can lead to data leaks, destructive mutations, or full machine compromise if the agent is induced to execute harmful commands or use overly-powerful integrations.

Deployments should explicitly evaluate their own risk profile and harden the execution harness where appropriate. Sortie does not mandate a single hardening posture, but deployments should not assume that tracker data, repository contents, prompt inputs, or tool arguments are fully trustworthy just because they originate inside a normal workflow.

Prompt injection risk: issue descriptions, comments, and labels are untrusted input that flows directly into agent prompts. Workflow prompts should include defensive instructions to reduce the risk of injected content manipulating agent behavior.

Possible hardening measures include:

- Tightening agent approval and sandbox settings instead of running with a maximally permissive configuration.
- Adding external isolation layers such as OS/container/VM sandboxing, network restrictions, or separate credentials beyond the built-in agent policy controls.
- Using `tracker.query_filter` to restrict which issues are eligible for dispatch (e.g., by label, component, epic, or other tracker-native criteria) so untrusted or out-of-scope tasks do not automatically reach the agent.
- Scoping the `tracker_api` tool (Section 10.4.5) so it can only read or mutate data inside the intended project scope, rather than exposing general tracker access.
- Reducing the set of registered tools, credentials, filesystem paths, and network destinations available to the agent to the minimum needed for the workflow.

The correct controls are deployment-specific, but deployments should document them clearly and treat harness hardening as part of the core safety model rather than an optional afterthought.

### 15.6 Workspace Content as Agent Configuration

Some agent runtimes read configuration out of the working directory they are handed, and start helper processes from what they find there. Sortie creates a workspace, checks repository content into it, and gives that directory to the agent as its working directory, so repository content reaches the runtime's configuration loader. Content that arrives with a checkout can therefore shape how the agent runs, and on a runtime that launches processes from configuration it can execute on the host under the account Sortie runs as.

Processes an agent runtime starts on its own behalf are not confined by that agent's sandbox setting. The sandbox governs commands the agent executes; it does not govern the transports and helper processes the runtime launches to serve the session. Nor does it govern whether workspace configuration is read at all. A runtime that gates that reading on a recorded trust decision consults the sandbox only when deciding whether to record trust for a path it has not seen before, so a restrictive sandbox prevents the decision rather than the loading. On a path that has never been trusted it therefore does protect: no decision is recorded, so nothing project scoped is read and no helper declared there is started. A path already carrying that decision keeps loading its configuration, and keeps starting the processes that configuration declares, whatever the sandbox says on the run that reaches it. The approval policy makes no difference to any of this. Tightening the sandbox setting therefore does not close the exposure, which is the limit of the hardening guidance above.

Sortie derives a workspace path from the issue identifier, so runs for the same unit of work share a path. A trust decision recorded against that path outlives both the run and the workspace content that earned it, so the first permissive run for an issue arms every later run at the same path.

The workspace Sortie creates and hands over is the boundary that matters, and two operator-facing controls act on it. Give the agent runtime a configuration home scoped to the run, so whatever it records while running dies with the run instead of accumulating in the operator's own configuration; this is demonstrated workable rather than theoretical. Control what lands in the checkout, because the exposure is bounded by who can place a file in the checked-out tree: a workflow that checks out contributor-supplied refs widens it well beyond one that builds only the default branch.

Sortie launches a local agent with the orchestrator's environment, so any credential present there is readable by an agent running with approvals disabled in a write-capable sandbox. Whether a tracker credential is present depends on how it is supplied. A value named indirectly through an environment variable, or supplied by an environment override, sits in the orchestrator's environment and is inherited. A value written literally into workflow configuration does not, and reaches the tracker client without passing through the environment, although it then sits in the configuration file instead.

Dispatching a run to a remote host bounds this differently. Only the variables the operator names plus the credential variables the agent kind declares, less the variables the operator disallows, cross to the remote host, delivered outside any process argument list, so the remote agent inherits that set rather than the orchestrator's environment, and the exposure is still the size of that set.

The sandbox does not help in either case. It governs filesystem and network reach, not what a process reads from the environment it was started with.

Two credentials of Sortie's own HTTP control surface (§13.7) are bounded on purpose. The operator's `server.control_token` never reaches an agent: every variable of the orchestrator's environment holding its value is removed at startup. Each work session instead receives `SORTIE_SESSION_TOKEN`, a token that authorizes only the per-issue control routes of its own issue and stops working when its run ends. A local agent can read that token, as it can read anything its wrapper holds, so the worst a leaked session token does is act on its own run, in ways the run's limits still bound and the run's state shows.

The `smolvm` execution environment (Section 15.8) moves this exposure off the host: the runtime, its configuration loader, and every helper it starts run in the issue's VM, with only the variables Sortie gives the VM.

### 15.7 Control API Exposure

The state-changing HTTP endpoints accept requests without a token only while no control token is set, and Sortie then refuses any bind address that is not a loopback address. A deployment that exposes the server, a container bound to `0.0.0.0` among them, sets `server.control_token`. Read-only endpoints keep no authentication: put the server behind a network boundary or an authenticating proxy when the dashboard, the runtime snapshot, or the metrics must not be readable.

Which runtimes read workspace configuration, which files they key on, and how any of this differs between their releases are adapter concerns and belong in the adapter notes.

### 15.8 Execution Environment Isolation

`environment.kind: smolvm` (Section 9.7) is Sortie's built-in sandbox. Its goal: no code from the workspace or the agent runs on the host, and that includes `git` reading a repository the guest controls. The `local` kind keeps the host-execution model of the sections above.

**What runs in the VM.** The agent, its credential verification session, auxiliary agent commands, the four hooks, self-review verification commands, reaction triage, and every `git` command that inspects the workspace. The host only reads the output bytes of the inspection commands, with the existing caps. There is no fallback to the host when the VM cannot be prepared.

**No host git on a guest repository.** The repository lives on the VM's disk. When `environment.smolvm.repository` is set, git data crosses only as bundles piped through a guest command, outside the workspace. The host imports exactly one ref, `refs/heads/<issue branch>`, from a bundle into its own bare mirror, after checking that the bundle names nothing else and stays under `max_bundle_mib`. Every host git command on the mirror runs with hooks disabled, the filesystem monitor off, and every received object checked, so a planted hook, filter driver, or `.gitattributes` entry in an exported commit runs nothing on the host. A planted fsmonitor or filter can still run in the guest, where the agent can run anything anyway; at worst it falsifies its own handoff evidence. Reading `.git` objects through a shared mount is never done.

**Credentials.** Sortie pushes with the host's own git credentials (SSH agent, credential helper, deploy key); they never enter the VM, and the only ref Sortie pushes is the issue branch, with a lease, so a commit someone else pushed is never overwritten. Prefer a deploy key or a token scoped to the one repository. An agent's API key enters only as a smolvm credential binding: the guest holds a placeholder, and the proxy substitutes the value only on HTTPS requests to the bound hosts. `pass_env` copies a host variable into the VM in plain text, by explicit opt-in. An agent credential variable that is set on the host and neither bound nor passed fails the dispatch preflight; it is never copied silently. No other host variable reaches the VM: hooks and inspection commands there inherit nothing from Sortie's process, `SORTIE_*` variables included. `SORTIE_SESSION_TOKEN` and `SORTIE_API_URL` never reach the VM; a host prefix keeps them on the host. The generated MCP configuration carries no env block for Sortie's own server, but servers an operator adds through an agent block's `mcp_config` are merged as written and run in the VM, so any value in their env blocks is visible there.

**Egress.** A VM reaches no network host unless `allow_hosts` lists it, plus the hosts of its credential bindings, which Sortie always adds because a binding without an allowlist opens the whole network. With `repository` set, the git host needs no entry, so git is not an exfiltration path. Every listed host still is one: a package registry, or the model API itself. Denials are logged by smolvm (`smolvm machine egress-events --name <vm>`); the VM name is in `/api/v1/state`.

**Host resources.** Exactly three: the issue's `.sortie` directory read-write, the issue's tool socket, and a read-only directory that holds only the Sortie binary. A guest write to `/opt/sortie` fails, also after a remount from inside the guest. No nested virtualization, system mounts, Docker socket, published ports, or other host path. SSH agent forwarding is an explicit opt-in (`ssh_agent`).

**The shared control directory.** Files the agent writes there are read on the host through `workspacekit`, with links refused and sizes capped (Invariant 7). A guest can plant a link there; it is refused, not followed. The environment record there is agent-writable, so removal never follows a record to the host (Invariant 11). The trusted SCM record lives in `.sortie-host`, outside the shared directory, so an agent cannot point CI polling, reviews, or auto-merge at another pull request.

**The tool socket.** Any process in the VM can connect to it, as any process the agent starts can already call its tools. Each socket serves only its own issue, and the tool server it starts runs on the host with that issue's identity. The workflow file, the tracker credentials, and the database stay on the host.

**What isolation does not cover.** Everything the agent commits reaches the upstream issue branch, including changes to CI configuration or to the repository's own sandbox setup; branch protection on the base branch stays the merge gate. Data can leave through any allowed host. The shared control directory can fill the host disk up to the workspace root's filesystem; Sortie never reads past its caps. Stopping a session ends the agent at once, with no graceful signal. The VM boundary is smolvm's; Sortie accepts only smolvm `1.26.x`, the release whose behavior was measured.
