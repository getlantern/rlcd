# rlcd

_Reinforcement learning for calibrated decisions_ is a trained language model that outputs decisions as structured data, not generated text/code. [^primer]

[^primer]: https://docs.typesafe.ai/introduction/machine-learning-primer

## Purpose

Minimal wrapper for an RLCD HTTP API, with package and CLI entry points. RLCD should be used over chat-focused LLM when the only output desired is a confidence-ranked decision against preexisting criteria.

## Agent Instructions

https://github.com/typesafe-ai/skills/blob/main/skills/typesafe-ai/SKILL.md

## Usage

| variable | purpose |
|---|---|
| `TYPESAFE_API_KEY` | (required) authentication key, paired to service backing `TYPESAFE_BASE_URL` |
| `TYPESAFE_BASE_URL` | Optional `https` API root. Use `https://openrouter.ai/api` for OpenRouter. |

| command | action |
|---|---|
| `rlcd ask` | Evaluate state from `stdin` or _file_ against a questions file. |
| `rlcd check FILE...` | Validate a questions file.  |
| `rlcd example` | Print an example questions file covering every question type. |

Stdin is read as one text state. Use `--lines` for one state per line, and `--state-format json` when stdin carries JSON or JSONL.

| exit code | cause |
|---|---|
| `0` | Every state was answered. |
| `1` | A state or the run failed; failed states still get an `{"error": ...}` line. |
| `2` | Invalid flags, configuration, or questions. |
| `130` | Interrupted. |

### CLI

A single simple state from stdin:
```sh
echo "Help! My payouts have been failing for 3 days." | rlcd ask --questions cmd/cli/example.toml | jq
```

Output
```json
{
  "model": "jev-1.13.0",
  "answers": {
    "department": {
      "type": "choice",
      "choice": "billing",
      "confidence": 0.91,
      "probabilities": {
        "billing": 0.93,
        "other": 0,
        "sales": 0,
        "technical": 0.07
      }
    },
    "frustration": {
      "type": "score",
      "score": 1.99,
      "confidence": 0.97,
      "probabilities": {
        "0": 0,
        "1": 0.02,
        "2": 0.97,
        "3": 0.01
      },
      "legend": {
        "0": "Calm",
        "1": "Mildly annoyed",
        "2": "Frustrated",
        "3": "Very angry"
      }
    },
    "is_spam": {
      "type": "noul",
      "noul": 0.09
    },
    "is_urgent": {
      "type": "noul",
      "noul": 0.95
    },
    "is_vip_issue": {
      "type": "noul",
      "noul": 0.98
    }
  },
  "usage": {
    "input_tokens": 516,
    "output_tokens": 117
  }
}
```

Process text against a Questions file, one state per line of `tickets.txt`:
```sh
rlcd ask --questions cmd/cli/example.toml --state tickets.txt --lines --out results.jsonl
```

Output (`results.jsonl`, one line per line of `tickets.txt`, in the same order)
```json
{"model":"jev-1.13.0","answers":{"department":{"type":"choice","choice":"billing","confidence":0.91,"probabilities":{"billing":0.93,"other":0,"sales":0,"technical":0.07}},"frustration":{"type":"score","score":1.99,"confidence":0.97,"probabilities":{"0":0,"1":0.02,"2":0.97,"3":0.01},"legend":{"0":"Calm","1":"Mildly annoyed","2":"Frustrated","3":"Very angry"}},"is_spam":{"type":"noul","noul":0.09},"is_urgent":{"type":"noul","noul":0.95},"is_vip_issue":{"type":"noul","noul":0.98}},"usage":{"input_tokens":516,"output_tokens":117}}
```

Act on an answer:
```sh
echo "Help! My payouts have been failing for 3 days." \
  | rlcd ask --questions cmd/cli/example.toml \
  | jq --exit-status '.answers.is_urgent.noul > 0.8' > /dev/null \
  && echo "page on-call" || echo "add to queue"
```

Output
```
page on-call
```

### `pkg`

```go
raw, err := os.ReadFile("cmd/cli/example.toml")
if err != nil {
	slog.ErrorContext(ctx, "reading questions", "error", err)
	os.Exit(1)
}
file, err := questions.Parse(raw)
if err != nil {
	slog.ErrorContext(ctx, "parsing questions", "error", err)
	os.Exit(1)
}

c := typesafe.NewClient(os.Getenv("TYPESAFE_API_KEY"))
resp, err := c.Ask(ctx, typesafe.Request{
	State:     "Help! My payouts have been failing for 3 days.",
	Model:     file.Model,
	Questions: file.Questions,
})
if err != nil {
	slog.ErrorContext(ctx, "asking typesafe", "error", err)
	os.Exit(1)
}

dept := resp.Answers["department"]
slog.InfoContext(ctx, "routed",
	"department", dept.Choice,
	"confidence", dept.Confidence,
	"urgent",     resp.Answers["is_urgent"].Noul,
)
```

Output
```
2026/10/15 09:12:00 INFO routed department=billing confidence=0.91 urgent=0.95
```

## Considerations

*Why `toml` input when docs show `json`?*

Provided `questions` may be frequently evaluated and edited by non-technical humans to align with business aims, and `toml` is both friendlier to read, and feasible for a hand-edit to produce a valid file.
