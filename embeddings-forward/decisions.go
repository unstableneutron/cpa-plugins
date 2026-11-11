package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// decisionsOnlyFields are OpenRouter Decisions request fields with no System
// One equivalent (routing preferences and observability metadata). The rest
// of the body is shared: OpenRouter documents TypeSafe bodies working on
// Decisions unchanged, and the Decisions response is a superset of System One's.
var decisionsOnlyFields = map[string]bool{"provider": true, "session_id": true, "trace": true, "user": true}

type field struct {
	key   string
	value json.RawMessage
}

type batchResult struct {
	result  upstreamResult
	outcome outcome
}

// decide answers every question, splitting by each provider's question cap.
// Batches failing with a retryable status move to the next provider; answers
// already received are kept and merged with their usage.
func (c call) decide(matches []match, top map[string]json.RawMessage, headers map[string][]string) directResponse {
	questions, err := orderedObject(top["questions"])
	if err != nil || len(questions) == 0 {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "invalid_questions", "questions must be a non-empty JSON object", "")
	}
	pending := questions
	answers := make(map[string]json.RawMessage, len(questions))
	var successes []upstreamResult
	var last upstreamResult
	for round := 0; ; round++ {
		for _, m := range c.h.keys.order(matches) {
			if len(pending) == 0 {
				break
			}
			batches := split(pending, m.provider.MaxQuestions)
			results := make([]batchResult, len(batches))
			limit := make(chan struct{}, m.provider.MaxConcurrency)
			var wg sync.WaitGroup
			for i, batch := range batches {
				wg.Add(1)
				limit <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-limit }()
					body := decisionBody(top, m, batch, len(batch) != len(questions))
					result, out := c.tryProvider(m, body, headers)
					results[i] = batchResult{result: result, outcome: out}
				}()
			}
			wg.Wait()
			var retry []field
			for i, batch := range batches {
				r := results[i]
				if r.outcome == outcomeFinal {
					c.h.log(c.callbackID, "warn", "embeddings-forward: request failed", map[string]any{"module": c.module, "provider": r.result.provider, "status": r.result.status})
					return clientResponse(r.result)
				}
				if r.outcome == outcomeSuccess {
					got, ok := parseAnswers(r.result.body)
					if ok {
						successes = append(successes, r.result)
						for _, q := range batch {
							if answer, found := got[q.key]; found {
								answers[q.key] = answer
							} else {
								retry = append(retry, q)
							}
						}
						continue
					}
					r.result.malformed = true
				}
				last = r.result
				retry = append(retry, batch...)
			}
			pending = retry
		}
		if len(pending) == 0 || round >= c.retries {
			break
		}
	}
	if len(pending) > 0 {
		c.h.log(c.callbackID, "warn", "embeddings-forward: request failed", map[string]any{"module": c.module, "provider": last.provider, "status": last.status, "unanswered": len(pending)})
		return clientResponse(last)
	}
	if len(successes) == 1 {
		return clientResponse(successes[0])
	}
	return directResponse{StatusCode: http.StatusOK, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: mergeDecisions(questions, answers, successes)}
}

func split(questions []field, size int) [][]field {
	if size <= 0 || len(questions) <= size {
		return [][]field{questions}
	}
	var out [][]field
	for start := 0; start < len(questions); start += size {
		out = append(out, questions[start:min(start+size, len(questions))])
	}
	return out
}

// decisionBody builds one provider request for a batch of questions.
func decisionBody(top map[string]json.RawMessage, m match, batch []field, batched bool) []byte {
	out := make(map[string]json.RawMessage, len(top))
	for key, value := range top {
		if m.provider.API == apiSystemOne && decisionsOnlyFields[key] {
			continue
		}
		out[key] = value
	}
	out["model"] = mustJSON(m.mapping.Name)
	encoded := make([]field, len(batch))
	for i, q := range batch {
		encoded[i] = field{key: q.key, value: q.value}
		if m.mapping.StringInstructions {
			encoded[i].value = stringInstructions(q.value)
		}
	}
	out["questions"] = encodeObject(encoded)
	if assets, ok := top["assets"]; ok && batched {
		// Upstreams reject assets that a request never references.
		out["assets"] = referencedAssets(assets, top["state"], out["questions"])
	}
	return mustJSON(out)
}

// stringInstructions JSON-encodes structured instructions for models that
// accept only string instructions.
func stringInstructions(question json.RawMessage) json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(question, &object) != nil {
		return question
	}
	instructions, ok := object["instructions"]
	if !ok || bytes.HasPrefix(bytes.TrimSpace(instructions), []byte(`"`)) {
		return question
	}
	var compact bytes.Buffer
	if json.Compact(&compact, instructions) != nil {
		return question
	}
	object["instructions"] = mustJSON(compact.String())
	return mustJSON(object)
}

func referencedAssets(assets, state, questions json.RawMessage) json.RawMessage {
	fields, err := orderedObject(assets)
	if err != nil {
		return assets
	}
	var kept []field
	for _, asset := range fields {
		if bytes.Contains(state, []byte(asset.key)) || bytes.Contains(questions, []byte(asset.key)) {
			kept = append(kept, asset)
		}
	}
	return encodeObject(kept)
}

func parseAnswers(body []byte) (map[string]json.RawMessage, bool) {
	var response struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if json.Unmarshal(body, &response) != nil || response.Answers == nil {
		return nil, false
	}
	return response.Answers, true
}

// mergeDecisions keeps the first response's metadata, orders answers like the
// request and sums numeric usage fields across batches.
func mergeDecisions(questions []field, answers map[string]json.RawMessage, results []upstreamResult) []byte {
	var top map[string]json.RawMessage
	_ = json.Unmarshal(results[0].body, &top)
	ordered := make([]field, 0, len(questions))
	for _, q := range questions {
		ordered = append(ordered, field{key: q.key, value: answers[q.key]})
	}
	top["answers"] = encodeObject(ordered)
	top["usage"] = sumUsage(results)
	return mustJSON(top)
}

func sumUsage(results []upstreamResult) json.RawMessage {
	type total struct {
		integer int64
		float   float64
		isFloat bool
	}
	var order []string
	totals := map[string]*total{}
	other := map[string]json.RawMessage{}
	for _, result := range results {
		var response struct {
			Usage map[string]json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(result.body, &response) != nil {
			continue
		}
		for key, raw := range response.Usage {
			if _, seen := totals[key]; !seen {
				if _, seenOther := other[key]; !seenOther {
					order = append(order, key)
				}
			}
			text := strings.TrimSpace(string(raw))
			if number, err := strconv.ParseFloat(text, 64); err == nil {
				t := totals[key]
				if t == nil {
					t = &total{}
					totals[key] = t
				}
				if integer, errInt := strconv.ParseInt(text, 10, 64); errInt == nil && !t.isFloat {
					t.integer += integer
				} else {
					if !t.isFloat {
						t.float, t.isFloat = float64(t.integer), true
					}
					t.float += number
				}
			} else if _, seen := other[key]; !seen {
				other[key] = raw
			}
		}
	}
	out := make([]field, 0, len(order))
	for _, key := range order {
		if t := totals[key]; t != nil {
			value := strconv.FormatInt(t.integer, 10)
			if t.isFloat {
				value = strconv.FormatFloat(t.float, 'g', -1, 64)
			}
			out = append(out, field{key: key, value: json.RawMessage(value)})
		} else {
			out = append(out, field{key: key, value: other[key]})
		}
	}
	return encodeObject(out)
}

// orderedObject decodes a JSON object preserving key order; a repeated key
// keeps its first position and last value, matching JSON decoders.
func orderedObject(raw json.RawMessage) ([]field, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	var fields []field
	index := map[string]int{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if i, ok := index[key]; ok {
			fields[i].value = value
			continue
		}
		index[key] = len(fields)
		fields = append(fields, field{key: key, value: value})
	}
	return fields, nil
}

func encodeObject(fields []field) json.RawMessage {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buffer.WriteByte(',')
		}
		buffer.Write(mustJSON(f.key))
		buffer.WriteByte(':')
		buffer.Write(f.value)
	}
	buffer.WriteByte('}')
	return buffer.Bytes()
}
