# Multiple Traffic Evidence Items per Finding

The finding details **Linked Traffic** section supports selecting items across pages, specifying roles and notes, sorting, and unlinking. The Traffic page also lets you select multiple records and link them to an existing finding in one operation. Evidence inherited from another task is read-only; edit it in the source task.

**Agent auto-bind traffic** is disabled by default in System Settings. Its read/write API field is `agent_traffic_binding` on `/api/settings`. Enabling it increases token usage from inspecting requests/responses, tool calls, and prompt instructions; Agents use the new setting on their next turn. When disabled, auto-binding parameters and the binding tool are hidden, auto-binding instructions are not injected, and running sessions cannot submit new automatic bindings. Manual binding, traffic capture, reading saved evidence, and exports are unaffected.

When enabled, the default flow is **record finding → automatically invoke the report Agent → verify and bind traffic → write the report against the latest evidence version**. The reporter preserves verification commands, key output, and existing real traffic IDs and roles in `evidence`. The report Agent uses finding details and execution records to verify them with `traffic_search` / `traffic_get`, calls `bind_finding_traffic`, then reads the latest `version` before saving the report. When the setting is disabled, the report Agent gets no additional raw traffic search/read tools and does not bind traffic automatically; it can still read manually bound snapshots and generate reports.

Existing callers remain compatible: `traffic_refs` / `evidence_hint_id` on `report_finding` can still explicitly bind evidence at submission time. Binding is optional: for non-HTTP findings such as TCP issues, or when traffic was not captured or an exact record cannot be found, callers can omit references and still submit findings and write reports. Preserve other verifiable evidence such as command output and logs; it is recommended to explain why no traffic was bound. No new required fields are introduced. Every submitted ID must be valid and its body complete. If any item fails, the current binding operation is rolled back; if explicit binding during submission fails, the entire submission is rolled back. Adding the same snapshot twice does not create another binding or overwrite its note.

## Agent IDs and Report Versions

`report_finding` adds an optional parameter; the array order defines the initial evidence order:

```json
{
  "traffic_refs": [
    {"traffic_id": "real-traffic-id", "role": "baseline", "note": "Normal authenticated request"},
    {"traffic_id": "another-real-traffic-id", "role": "proof", "note": "Reproduction request"}
  ]
}
```

Roles are `baseline` (normal control), `proof` (proof of the finding), `verification` (additional verification), and `supporting` (supporting evidence, the default). Verify real records with `traffic_search` / `traffic_get` first; domain and time are only for narrowing candidates and do not establish task ownership.

The prompt is based on [CyberStrikeAI's finding-reporting tool guidance](https://github.com/RuoJi6/CyberStrikeAI/blob/54d56774b8bd285817d16d48b70a4a5e6e0963f7/internal/app/vulnerability_tools.go), adapted to this project's optional-binding convention; it does not require an explanation when no traffic is attached. Do not guess IDs or repeat probes solely to obtain a traffic capture.

The first response line remains `finding recorded: <exploration node ID>`. It is followed by JSON containing the separate finding record's `finding_id`, the exploration node's `finding_node_id`, and a binding summary.

- `get_finding_traffic(finding_id)` takes the **separate finding record ID** and returns an ordered list and `version`. Pass `binding_id`, `side=request|response`, `offset`, and `length` to read in chunks of at most 8192 bytes.
- `update_finding_report` **continues to use the exploration node ID** as `finding_id`. The new `evidence_version` field must contain the version actually read; if evidence changes during generation, writes using the old version are rejected and the evidence must be re-read before regenerating the report.
- Legacy report calls that omit the version do not claim to cover existing traffic evidence. After a binding, note, role, or ordering changes, existing reports are marked as needing an update.

When auto-binding is enabled, `add_hint` / `add_task_hint` can save `traffic_refs` on a single hint or on each item in a batch of `hints`. A Planner reporting on its behalf can pass `evidence_hint_id` to explicitly select references from a hint in the current task; inherited hints cannot be referenced. The system does not guess bindings from domains, timestamps, or browsing history. A failed submission does not create a partial finding or trigger a report prematurely.

Use `bind_finding_traffic(finding_id, traffic_refs)` to add missing bindings to an existing finding without re-registering it. `list_findings` / `list_task_findings` / `node_detail` / `get_task_node_detail` return explicit `finding_id` and `finding_node_id` values; the legacy `id` retains its exploration-node meaning.

At startup, only optional properties are added to legacy tool schemas, and the original default traffic tools are extended to the report Agent; the report Agent also receives the binding tool by default. Custom tool bindings, prompts, descriptions, and enabled states are preserved. Instructions are added after final tool assembly: the reporting role hands off existing evidence, and the report Agent verifies and binds it before writing the report. If a platform chat lacks task context, it should hand off to a task Agent through structured instructions rather than report directly. Existing evidence should be handed off before declaring a task complete; waiting for a traffic capture is not mandatory. A failed `report_finding` does not trigger the report Agent.

## API

Base path: `/api/exploration/findings/{finding_id}/traffic`, using the separate finding ID. Existing authentication applies; `context_task` checks task visibility and read-only inheritance.

| Method / Relative path | Request / Response |
| --- | --- |
| `GET` | Ordered summaries, evidence version, and report version |
| `POST` | Append the full `{"traffic_refs":[...]}` batch |
| `PATCH /{binding_id}` | `{"version":1,"role":"proof","note":"Description"}` |
| `DELETE /{binding_id}` | `{"version":1}` |
| `PUT /order` | `{"version":1,"binding_ids":["2","1"]}`; the list must be complete |
| `GET /{binding_id}` | Snapshot metadata and bounded body preview |
| `GET /{binding_id}/body` | `side`, `offset`, `length`; `download=1` downloads the complete original bytes |

Version/order-set conflicts and writes during archiving return `409`; writes to inherited findings return `403`; missing bindings or bindings that do not belong to the finding return `404`. Traffic/attachment read and validation failures return explicit errors.

## Storage and Migration

An idempotent PostgreSQL migration runs at startup, adding `traffic_evidence_snapshots`, `finding_traffic_bindings`, and `findings.evidence_version` / `report_evidence_version` (default 0). Existing bindings are not inferred from historical text.

Snapshots store the original traffic ID, capture time, URL, method, status, request/response headers, body length, and SHA-256. Bodies are stored by hash at `<data>/evidence/blobs/<first-two-characters>/<hash>.bin`, separate from the cleanable `data/traffic` directory; multiple findings can share snapshots/bodies. Snapshots have no content-update API. Reads and exports fail if validation detects a mismatch.

Under the original traffic write lock, the complete body is read, including large body blobs and legacy directory records. Files are persisted and validated first, then exploration nodes, intent relationships, findings, snapshots, and bindings are written in a single PostgreSQL transaction. The planner is notified only after commit. A failure may leave unreferenced files, but does not create partial business records.

PostgreSQL advisory lock `7337741004` coordinates evidence files and SQL references; a task row lock prevents evidence changes after archiving is queued. Restore holds the evidence lock from body installation through metadata commit. Deleting a finding cascades to its bindings.

The cleaner runs hourly and reclaims only unreferenced content not involved in an in-progress operation, with a delay of at least 24 hours. Regular traffic cleanup does not touch the evidence directory. Back up PostgreSQL and `data/evidence` together when backing up live data.

## Export and Archiving

Markdown exports include an ordered evidence list and version; JSON exports include metadata; CSV exports add the count and binding IDs. `md-zip` retains the finding Markdown and includes:

```text
evidence/<finding_id>/<binding_id>/
  manifest.json
  request.http
  response.http
  request.bin
  response.bin
```

The Markdown references traffic records with relative links. Before serving a download, attachments are copied, hash-validated, compressed, synced to disk, and all ZIP entries are read and CRC-validated. Missing or corrupted data fails the entire download. Full attachments preserve their original binary bytes.

Archive v3 collects snapshots and bodies through finding bindings, without relying on the original traffic records or domains. Hot data is cleaned only after package validation; shared evidence is retained. During restore, bodies are validated before installation, then metadata and bindings are restored transactionally, with failed restores eligible for retry. v1/v2 archives remain restorable, with missing new fields explicitly set to 0.

## Validation and Limitations

Create a separate, fresh PostgreSQL test database for each test package and specify it with `ARTEX_PG_DSN` to prevent leftover tasks/model fixtures from triggering background runs. Run all tests in the relevant packages and confirm that none were skipped because configuration was missing:

```sh
# Before running each package, set ARTEX_PG_DSN to its separate test database; explicit configuration failures must be reported.
go test ./<package> -count=1
go test -race -p 1 ./evidence ./db ./agent ./server -run 'TestEvidence|TestFindingTraffic|TestFindingEvidence|TestReportFindingAtomicContract|TestTaskArchive'
```

Frontend validation includes `npx tsc --noEmit`, Biome checks on affected files, a Webpack build, and static export with `NEXT_EXPORT=1`. Use a separate cache directory to avoid overwriting a running development service.

Local end-to-end acceptance uses separate ports, controlled HTTP/domain HTTPS targets, and a temporary data directory. It covers both binding entry points, cross-page selection, sorting/notes, error messages, inherited read-only behavior, downloads, and export after deleting the original traffic, archiving, reclaiming hot bodies, restoring, and validating hashes.

The initial version uses a global evidence coordination lock; other evidence operations may wait during large binding/export operations or slow attachment downloads. Evidence cannot be reconstructed if the traffic record or complete body is missing. This feature does not change the capture setting or address certificate issues for direct HTTPS access by IP.
