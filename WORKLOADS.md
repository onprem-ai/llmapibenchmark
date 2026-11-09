# Workloads

A workload is one versioned way of measuring an OpenAI-compatible endpoint:
its request shapes, its concurrency ladder and the size of each level's
sample. Run one with `--concurrency auto --format jsonl --workload <id>`.
The definitions live in `internal/ladder/workloads.go`, and a test pins each
released definition: one never changes without its version.

## Comparability

Two results compare when their workloads share a name and a major version.

| Part | Changes when | Results |
|---|---|---|
| major | lengths, distribution, seed, sample size, a figure's definition, the measurement method | do not compare |
| minor | more levels on the ladder, a new series | compare at the levels and series both have |
| patch | a fix that changes no figure | compare |

## Version 1.0.0 workloads

| Workload | Prompt tokens | Answer tokens | Plateau decided by |
|---|---|---|---|
| `max-prefill/1.0.0` | 8192 | 64 | prefill tokens per second |
| `max-decode/1.0.0` | 256 | 1024 | decode tokens per second |
| `mixed-load/1.0.0` | 128 to 8192, log-uniform, seed 1 | 64 to 1024, log-uniform, seed 1 | both |

Common to all three:

- Every prompt starts with text no other request sent, so no prefix cache
  answers it. Lengths are in the engine's own tokens, sized from one
  calibration request.
- Every request asks the engine to ignore the end of sequence
  (`ignore_eos`), so every answer has the length it asked for.
- The ladder is 1, 2, 4, 8, 12, 16, 20, 24, 32, 40, 48, 64, 80, 96, 112, 128.
  Every level up to 32 is measured; after it, the workload stops when two
  levels in a row gained no more than 5 percent over the best earlier level
  (its plateau measure), or at 128. The plateau concurrency is the lowest
  level that reached 95 percent of the best (for "both", the later of the
  two measures' levels).
- A prompt that would not fit the model's context with the longest answer is
  shortened (`--max-context-tokens`).

## How a level is measured

- Exactly `concurrency` requests are in flight: a new one starts the moment
  one ends.
- The window starts once every slot finished its first request (the
  warm-up). The level's sample is the first max(20, 2 x concurrency)
  requests sent after it; the window ends when the last of them ends.
- Throughput counts what the replica did inside the window: a request's
  prompt tokens at its first token, its generated tokens after the first
  spread evenly over its generation, divided by the window's length.
- Latencies and lengths come from the sample's requests.

## Figures of a level

| Field | Meaning |
|---|---|
| `window_seconds` | the window's length |
| `requests`, `requests_failed` | the sample |
| `requests_per_second` | requests completed inside the window per second |
| `prefill_tokens_per_second` | prompt tokens processed inside the window per second |
| `decode_tokens_per_second` | tokens generated inside the window per second |
| `decode_tokens_per_second_cv` | coefficient of variation of decode throughput over the window's four quarters |
| `time_to_first_token_seconds_p50/p90/p95` | from sending a request to its first token |
| `inter_token_latency_seconds_p50/p90/p95` | a request's generation time divided by its tokens after the first |
| `request_latency_seconds_p50/p90/p95` | from sending a request to its last token |
| `prompt_tokens_mean/stddev`, `completion_tokens_mean/stddev` | the lengths served, in the engine's tokens |

Percentiles are nearest-rank. A figure a level could not measure is null.
