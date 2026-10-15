# typesafe

A minimal Go client for the [TypeSafe System One API](https://docs.typesafe.ai/api). It sends a `state` and a set of typed questions, and gets back one typed answer per question. It uses only the standard library.

```go
import "github.com/getlantern/rlcd/pkg/typesafe"
```

## Questions

Each question is keyed by a name you choose; its answer comes back under the same key. Keys are never sent to the model, so put the full meaning in the question itself.

| Constructor | Asks | Read from the answer |
|---|---|---|
| `Noul(instr)` | Is this true? | `Noul`, the probability of yes (0 to 1) |
| `NoulWith(instr, NoulCriteria{True, False})` | Is this true, with each outcome described? | `Noul` |
| `Choice(instr, map[string]any{...})` | Which one option? A `nil` description means the name speaks for itself. Up to 255 options. | `Choice`, `Confidence`, `Probabilities` |
| `Score(instr, levels...)` | Where on these ordered levels? Levels are indexed from 0; 2 to 10 levels. | `Score`, `Confidence`, `Probabilities`, `Legend` |

`State`, instructions, and criteria can each be a string, an object, or an array. Use structured values when the input has several parts, and refer to nested fields in backticks, for example `` `customer.plan` ``.

`Answer` is one struct for all three types; its `Type` field says which fields are set. For Score answers, `Probabilities` and `Legend` are keyed by the level index as a string (`"0"`, `"1"`, ...).

## Examples

### Minimal

```go
ctx := context.Background()
c := typesafe.NewClient(os.Getenv("TYPESAFE_API_KEY"))

resp, err := c.Ask(ctx, typesafe.Request{
	State: "Help! My payouts have been failing for 3 days.",
	Questions: map[string]typesafe.Question{
		"is_urgent": typesafe.Noul("Does this convey urgency?"),
	},
})
if err != nil {
	slog.ErrorContext(ctx, "asking typesafe", "error", err)
	os.Exit(1)
}
slog.InfoContext(ctx, "answered",
	"is_urgent", resp.Answers["is_urgent"].Noul, // 0.95
)
```


### Kitchen Sink

```go
c := typesafe.NewClient(os.Getenv("TYPESAFE_API_KEY"))
c.Model = "jev-latest"
c.HTTP = &http.Client{Timeout: 10 * time.Second}

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

resp, err := c.Ask(ctx, typesafe.Request{
	// State can be a string, object, or array.
	State: map[string]any{
		"customer": map[string]any{"plan": "pro", "tenure_months": 14},
		"messages": []string{
			"I was charged twice this month.",
			"Still waiting on a reply. This is ridiculous.",
		},
	},
	Questions: map[string]typesafe.Question{
		"is_urgent": typesafe.NoulWith(
			"Does `messages` convey urgency?",
			typesafe.NoulCriteria{
				True:  "Explicitly time-sensitive or escalating",
				False: "No urgency expressed",
			},
		),
		"department": typesafe.Choice("Which team should handle this?",
			map[string]any{
				"billing":   "Payments, invoicing, refunds",
				"technical": "Bugs, outages, integrations",
				"other":     nil, // described by its name alone
			},
		),
		"frustration": typesafe.Score("How frustrated is the customer?",
			"Calm", "Mildly annoyed", "Frustrated", "Very angry",
		),
		// Instructions can be structured; reference fields in backticks.
		"is_vip": typesafe.Noul(map[string]any{
			"vip_policy": "Pro plan and at least 12 months of tenure",
			"question":   "Does `customer` meet `vip_policy`?",
		}),
	},
})
if err != nil {
	var apiErr *typesafe.APIError
	if errors.As(err, &apiErr) {
		// A 422 body names the invalid field.
		slog.ErrorContext(ctx, "typesafe rejected request",
			"status",     apiErr.Status,
			"request_id", apiErr.RequestID,
			"body",       apiErr.Body,
		)
	} else {
		// Network error, timeout, or cancellation.
		slog.ErrorContext(ctx, "asking typesafe", "error", err)
	}
	os.Exit(1)
}

dept := resp.Answers["department"]
if dept.Confidence < 0.6 {
	slog.InfoContext(ctx, "unsure, routing to triage",
		"probabilities", dept.Probabilities,
	)
} else {
	slog.InfoContext(ctx, "routing", "department", dept.Choice)
}

f := resp.Answers["frustration"]
slog.InfoContext(ctx, "answered",
	"frustration",   f.Score,
	"legend",        f.Legend,
	"urgent",        resp.Answers["is_urgent"].Noul > 0.8,
	"vip",           resp.Answers["is_vip"].Noul > 0.5,
	"input_tokens",  resp.Usage.InputTokens,
	"output_tokens", resp.Usage.OutputTokens,
)
```
