// Package typesafe is a minimal client for the TypeSafe System One API.
//
// See https://docs.typesafe.ai/api for the wire format.
package typesafe

import "encoding/json"

// Request evaluates State against named Questions. Answers come back
// under the same keys; keys are never sent to the model.
type Request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// TsType is the discriminator shared by questions and answers.
type TsType string

const (
	TypeNoul   TsType = "noul"
	TypeChoice TsType = "choice"
	TypeScore  TsType = "score"
)

// Question is one typed judgment.
// Instructions and Criteria may be strings, objects, or arrays.
type Question struct {
	Type         TsType `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Noul asks a yes/no question.
func Noul(instructions any) Question {
	return Question{Type: TypeNoul, Instructions: instructions}
}

// NoulCriteria describes what a yes and a no mean.
type NoulCriteria struct {
	True  any `json:"true,omitempty"`
	False any `json:"false,omitempty"`
}

// NoulWith asks a yes/no question with described outcomes.
func NoulWith(instructions any, criteria NoulCriteria) Question {
	return Question{Type: TypeNoul, Instructions: instructions, Criteria: criteria}
}

// Choice picks one option. A nil description means the option is
// interpreted by its name alone. The API accepts up to 255 options.
func Choice(instructions any, options map[string]any) Question {
	return Question{Type: TypeChoice, Instructions: instructions, Criteria: options}
}

// Score rates along ordered levels, indexed from zero.
// The API accepts 2 to 10 levels.
func Score(instructions any, levels ...any) Question {
	return Question{Type: TypeScore, Instructions: instructions, Criteria: levels}
}

// Response holds one Answer per question key.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Usage reports token counts for a request.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Answer is the union of all answer types; Type says which fields apply.
//
//   - noul:   Noul
//   - choice: Choice, Confidence, Probabilities
//   - score:  Score, Confidence, Probabilities, Legend
//
// Score probability and legend keys are level indexes as strings.
type Answer struct {
	Type          TsType             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Legend        map[string]any     `json:"legend"`
}

// MarshalJSON writes only the fields that apply to the answer's Type,
// matching the API's wire format.
func (a Answer) MarshalJSON() ([]byte, error) {
	switch a.Type {
	case TypeNoul:
		return json.Marshal(struct {
			Type TsType  `json:"type"`
			Noul float64 `json:"noul"`
		}{a.Type, a.Noul})
	case TypeChoice:
		return json.Marshal(struct {
			Type          TsType             `json:"type"`
			Choice        string             `json:"choice"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		}{a.Type, a.Choice, a.Confidence, a.Probabilities})
	case TypeScore:
		return json.Marshal(struct {
			Type          TsType             `json:"type"`
			Score         float64            `json:"score"`
			Confidence    float64            `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
			Legend        map[string]any     `json:"legend"`
		}{a.Type, a.Score, a.Confidence, a.Probabilities, a.Legend})
	}
	// alias drops this method so unknown types marshal every field.
	type alias Answer
	return json.Marshal(alias(a))
}
