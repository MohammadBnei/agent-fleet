package ratings

import "testing"

func res(mu map[string]ModelUse) Result { return Result{ModelUsage: mu} }

func TestDominantModelIsLargestOutputWithStableTies(t *testing.T) {
	if got := DominantModel(map[string]ModelUse{"haiku": {OutputTokens: 5}, "glm": {OutputTokens: 900}}); got != "glm" {
		t.Fatalf("got %q", got)
	}
	if got := DominantModel(map[string]ModelUse{"b": {OutputTokens: 1}, "a": {OutputTokens: 1}}); got != "a" {
		t.Fatalf("tie: got %q", got)
	}
	if got := DominantModel(nil); got != "" {
		t.Fatalf("empty: got %q", got)
	}
}

func TestDeltasSubtractAndTreatACounterDropAsARestart(t *testing.T) {
	d := Deltas([]Result{
		res(map[string]ModelUse{"m": {InputTokens: 100, OutputTokens: 10, CostUSD: 1}}),
		res(map[string]ModelUse{"m": {InputTokens: 250, OutputTokens: 30, CostUSD: 3}}),
		res(map[string]ModelUse{"m": {InputTokens: 40, OutputTokens: 5, CostUSD: 0.5}}), // resumed pod
	})
	want := []int64{10, 20, 5}
	for i, w := range want {
		if d[i]["m"].OutputTokens != w {
			t.Errorf("result %d output = %d, want %d", i, d[i]["m"].OutputTokens, w)
		}
	}
	if d[1]["m"].InputTokens != 150 || d[2]["m"].CostUSD != 0.5 {
		t.Errorf("deltas = %+v", d)
	}
}

func TestMostUsedAcrossAModelSwitch(t *testing.T) {
	got := MostUsed([]Result{
		res(map[string]ModelUse{"claude-opus-5": {OutputTokens: 50}}),
		res(map[string]ModelUse{"glm": {OutputTokens: 80}}),
		res(map[string]ModelUse{"glm": {OutputTokens: 120}}),
	})
	if got != "glm" {
		t.Fatalf("got %q, want glm (80+40 > 50)", got)
	}
}

func TestParseResultReadsTheSdkShape(t *testing.T) {
	r, ok := ParseResult(`{"isError":true,"usage":{"input_tokens":3,"cache_read_input_tokens":4},"modelUsage":{"m":{"inputTokens":1,"outputTokens":2}}}`)
	if !ok || !r.IsError || r.Usage.Prompt() != 7 || r.ModelUsage["m"].OutputTokens != 2 {
		t.Fatalf("%+v ok=%v", r, ok)
	}
	if _, ok := ParseResult("not json"); ok {
		t.Fatal("garbage parsed")
	}
}

func TestResultTurnPrefersTheStampedModel(t *testing.T) {
	cum := map[string]ModelUse{"old": {OutputTokens: 900}, "new": {OutputTokens: 100}}
	if got := (Result{Model: "new", ModelUsage: cum}).Turn(); got != "new" {
		t.Errorf("stamped: %q", got)
	}
	d := map[string]ModelUse{"old": {OutputTokens: 1}, "new": {OutputTokens: 50}}
	if got := (Result{ModelUsage: cum, ModelUsageDelta: d}).Turn(); got != "new" {
		t.Errorf("delta: %q", got)
	}
}
