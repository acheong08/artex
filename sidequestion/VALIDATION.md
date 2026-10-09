# `/btw` Validation Record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

An isolated PostgreSQL test database and data directory were used. Real model credentials were injected only into the isolated test environment and were not written to code or this record. The product's default model was not changed. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

Actual model conversations, returned objects, engineering assertions, and the original Qwen review text are stored in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering Checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Structured messages and deep-copying tool parameters | Passed | `TestCheckpointDeepCopyAndBoundaries` |
| Summary/compaction requests do not overwrite snapshots; complete responses and terminal states publish; partial responses are excluded | Passed | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Identity of the actual model-pool member | Passed | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-pair replay, budget trimming, and overflow errors | Passed | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main/side concurrency and bidirectional cancellation isolation | Passed | Blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming/non-streaming, and retaining usage on failure | Passed | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool; main transcript/activity isolation | Passed | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subtests |
| Persistence, pagination, idempotency, and retaining partial answers across restart | Passed | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear/late-write races, parent-resource deletion, and version comparison | Passed | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent/Worker archive and restore, v1/v2/v3 | Passed | `TestSideTaskArchiveVersions` |
| Three parent endpoints, authentication, resource ownership, and logical Worker deletion | Passed | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Side questions during a busy main conversation; independent SSE reconnect/disconnect, cancellation, and clearing | Passed | `TestSideHTTPBusyIsolationClearAndReconnect` |
| One concurrent request per parent conversation / four globally | Passed | Two `TestSideHTTP…` cases |
| Snapshot persistence before admission, follow-up after restart, and preventing fabricated snapshots for old conversations | Passed | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Rejecting continuation after a cached profile is deleted or its model changes | Passed | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Canceling before archiving and waiting for the final answer and usage to persist | Passed | `TestSideTaskDrainPersistsBeforeArchive` |
| Recording usage exactly once and attributing it to the side question when a streaming consumer cancels early | Passed | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| Restored Worker runtime/deadline context continues publishing new snapshots after restart | Passed | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race checks for related packages | Passed | Commands below |
| TypeScript and production build | Passed | `npx tsc --noEmit`, `npm run build` |
| Biome check for added frontend modules | Passed | `biome check`, 3 added modules |

The automated checks can be reproduced by setting `ARTEX_PG_DSN` to a separate disposable database (do not point it at a production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression suite was not entirely green: two existing tests in the `server` package failed during temporary-directory cleanup, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

After exporting source from the unmodified baseline above and rerunning the `server` package in the same isolated environment, the same two cleanup failures reproduced. The baseline run also had a target-node-count assertion failure in `TestCoreTaskLifecyclePG`; the final `server` regression run did not. Other packages passed, as did the side-question cases and race checks. Baseline failures were not counted as passing acceptance checks, and existing assertions were not changed to mask them.

The Next.js build emitted existing warnings about multiple lockfiles/workspace-root inference, but completed successfully and generated all pages.

## Browser Checks

Using the Codex In-app Browser connected to a separate local Go service and Next.js development server, the following manual automated interactions were performed on desktop and at a narrow-screen size of 390 × 844; screenshots and browser logs were checked:

- Enter `/btw` during regular chat; main content and side question display simultaneously, and the desktop sidebar works.
- Ask follow-up questions; stopping the side question retains the generated partial answer while the main flow continues.
- Close the panel while the request continues, then reopen it to restore the completed answer; after refreshing, an empty `/btw` restores history.
- Drawer input, buttons, history, and close controls work on a narrow screen with no horizontal overflow.
- Clear uses a confirmation dialog; afterward, history is gone while the main transcript and snapshot remain.
- Ask questions in a task MainAgent and two separate Workers and switch between them; Agent labels and history do not cross over.
- A blocking local model fixture keeps a Worker running. Submit `/btw` from the Worker's main input, then stop the side question; the Worker still shows its live status and its own pause button, while the side question retains a partial answer.
- Browser error/warning logs are empty.

Controlled fixtures verify concurrency timing precisely and do not depend on real model output speed. During debugging, two checks while a Worker was running did not create a valid concurrency window (the task had ended / the answer had finished early). The fixture was corrected and the checks were rerun successfully; the initial attempts are not counted as passes.

## Real Model Conversations

The preferred model, `grok-4.6`, was probed through the OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200, model name `grok-4.6`, and `READY` in 2.82 seconds. Since the preferred model was available, the Tingly `glm` or Zhipu `glm-5.3` fallback chains were not enabled; neither fallback service was verified in this run.

| Scenario | Actual result |
| --- | --- |
| Ask about assets, objectives, and a marker while the main conversation is running | Returned `redhaze.top`, the homepage-read and summary objectives, and `BTW-REAL-0910`; side question completed in 16.97 seconds |
| Ask for the basis of a tool call after the main conversation reads the homepage | Correctly cited WebFetch 200, curl redirects 301 → 302 → 200, and the page title; 7.24 seconds |
| Ask the side question to create a test file with Bash | Execution was refused and the target file was not created; 7.74 seconds |
| Confirm that a completed side question does not change main context | Main transcript SHA-256 and main activity record remained unchanged; side-question tool executions: 0 |
| Ask a follow-up after actually stopping/restarting the Go service | Retained the previous 3 side-question history items and answered about assets, marker, and title directly from the persisted snapshot, without rerunning the main Agent |
| Use a Grok non-streaming configuration in a new conversation | Correctly answered about the asset and `ATOMIC-0910`; returned and saved usage: input 11734, output 138, cache_read 11520 |

For the asset case, the main conversation used WebFetch and Bash/curl to read the public homepage at `https://id.redhaze.top/home`. Its exact page title was “红幕科技 RedHaze Group · 全球综合集团门户”. Bash staged the response in a local test file; no writes were made remotely. This is verified separately from the fact that the side question executed no tools.

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming response did not return usage. A separate direct request with `stream_options.include_usage=true` confirmed HTTP 200, 12 data frames, and 0 usage frames. Therefore, 0 in the streaming test means that the endpoint did not provide usage; it does not mean that no usage was billed. Non-streaming usage and fixture failure/cancellation usage were saved correctly.

## Qwen Review

The review model was `qwen-flash`, using the OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`; it returned HTTP 200. It was given the first three real side-question conversations, the main-session tool evidence, and engineering assertions. It returned `verdict: accept` and `concerns: []`, finding that the answers matched the asset, marker, and page-read evidence, and that refusing side-question tool execution complied with the constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review did not include the later service-restart and non-streaming tests. Qwen's summary of "no writes" was too broad: the main-session curl did create a local temporary response file, as noted above. Concurrency, zero tool execution, and transcript isolation were evaluated through engineering assertions; model review only assisted with answer quality.
