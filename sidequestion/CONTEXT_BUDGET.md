# Side-Question History and Context Budget

Fixed on 2026-09-11. The original implementation treated the character count of request JSON as the token count and inherited the main task's 32K output reserve, so it prematurely rejected valid side questions when the context contained substantial HTML, JS, or tool results.

## Review of Open-Source Implementations

- [Grok CLI side-question context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts excerpts from recent user and assistant text, with a character budget of about 2,000 and at most 400 characters per item. This path does not maintain continuous side-question history.
- [Grok CLI independent request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): uses an independent cancellation signal; sets a 2,048-token output limit when supported by the model; provides no tools.
- [Grok CLI main-session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, preserves recent content, updates an existing summary with new content, and handles truncation across turns.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): estimates the full request, reserves output/buffer space, combines recent content with a rolling summary, and summarizes without tools. Its default recent-content budget is 8,000 tokens, with a 4,096-token summary output limit.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): attempts overflow recovery only before assistant output begins; the retried call does not enter the same overflow-recovery path again.

ARTEX adopts the separate output budget, recent content plus rolling summary, and bounded recovery approach. It retains norma v0.3.6 structured messages and tool pairing rather than copying Grok's text excerpts; OpenCode's main-session compaction events are not written to ARTEX's main transcript.

## Request Budget and Execution

- Messages use norma's UTF-8 byte-based estimate per content block with a 4/3 margin; system prompts, tool schemas, and message framing overhead are also included. This estimate is not an exact model token count.
- Side-question output defaults to at most 8,192 tokens and cannot exceed the output limit configured for the main profile. Set the 256–32,768-token limit with the service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS`; this does not change the product's default model or main-task parameters.
- The input budget is the context window minus the output limit and safety margin. Unknown context windows use the platform default of 200K. The safety margin is 5% of the window, with a minimum of 128 and maximum of 8,192 tokens.
- Successful Q&A is loaded in batches of at most 20, ordered by increasing ordinal. At most 20 pairs are retained verbatim, with a token budget of at most one quarter of the input budget and no more than 16K.
- Older Q&A is incorporated into a rolling summary. The summary includes historical sources and context time. Historical assistant answers are not new tool evidence; when they conflict, the latest main snapshot takes precedence.
- If the main context is still too long, only old messages in the side-question copy are summarized, retaining up to 8K tokens of recent content. Truncation never splits a tool call from its result. An oversized pair is summarized as a whole.
- Summary input is split into UTF-8-safe chunks according to the actual remaining window, with an output limit of 2,048 tokens. Empty, truncated, tool-calling, or over-budget summaries are not cached. A side question makes at most 12 summary calls and shares the same 120-second timeout; exceeding the limit fails explicitly rather than looping indefinitely.
- If the model first reports a context overflow before producing text or tool calls, the request is reduced further and retried at most once. Recovery stops immediately if the estimated size has not decreased. Other model errors and partial streaming output do not trigger recovery.
- All reported usage, including summaries, failed attempts, and cancellations, is added to the same side-question request. If a Provider does not return usage, only zero can be recorded; estimates are not presented as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores summaries of older Q&A, the covered ordinal, and main-context summaries cached by snapshot identity. `side_question_requests.context_info` stores the preparation stage, number of pairs actually replayed, summary usage, and budget estimates.

Summaries are saved only while the original request is still running and the cleanup version matches. Clearing also removes cached summaries, and late writes cannot restore cleared data. A new snapshot does not reuse summaries from an old snapshot. Summary fields are saved in v3 task archives; missing fields in older v3 archives are restored as empty objects. v1/v2 remain compatible.

POST first admits and returns the request; preparation and compaction run in the background without holding the admission lock or a database transaction. SSE/history shows the preparation, Q&A organization, copy compaction, and answering stages. Compaction failures are saved as the request's failed terminal state. The frontend retains errors and restores the failed question as a draft, without obscuring the input with a toast; history polling no longer clears submission errors.

## Validation Record

- Replay of 19/20/21/50 pairs, retention of older conclusions across the 20-pair boundary, and reuse of summary cache after restart: automated checks passed.
- Very long Chinese answers and code context, chunked request budgets, tool pairing, snapshot immutability, and cache invalidation on new snapshots: automated checks passed.
- Summary failures/cancellation/truncation/oversize/tool calls, clear races, call limits, single-attempt overflow recovery, and no retry on partial streams: automated checks passed.
- Pagination, restart, v1/v2/v3 archives, archive/restore of summary cache and budget metadata, older v3 archives missing new fields, and 20 parent conversations sharing four concurrency slots in an isolated PostgreSQL database: passed.
- Go candidate service build, frontend TypeScript check, Biome check of modified components, and Next.js production build in a separate directory: passed.
- The built-in browser used a separate UI fixture at 1280×720 and 390×844 to verify the organization stage, summary-scope notice, draft restoration after failure, no toast errors, no horizontal overflow, and no console errors. The temporary fixture was removed.
- Read-only replay of an existing local Worker snapshot passed the new budget checks; for example, the 293,085-character snapshot for Worker #3 is no longer incorrectly rejected based on a local character count. No external model call was made.
- An attempted real-conversation test sending a private Worker snapshot to Grok was blocked by automatic approval review. It was not run and is not counted as a passing test.
- On 2026-09-11 at 00:37, the local backend was restarted at the user's request using the existing database, data directory, and login configuration. The running executable's SHA-256 matched the candidate binary; `/api/health` returned successfully through both the backend and frontend proxy.

Validation commands (use only a separate test database):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build used a separate copy to avoid overwriting the current preview's `.next`. The candidate service was located at `/private/tmp/artex-btw-budget-candidate`, copied to `/private/tmp/artex-btw-preview/artex`, and started; the original binary backup was `artex.before-context-budget` in the same directory.
