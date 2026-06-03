// Copyright observIQ, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"gopkg.in/yaml.v3"
)

// newULID returns a 26-character Crockford base32 identifier from 128 random
// bits. Bindplane resource IDs are ULIDs; uniqueness is what matters here, so a
// random (non-monotonic) value is sufficient.
func newULID() string {
	const enc = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	n := new(big.Int).SetBytes(b[:])
	mod := big.NewInt(32)
	rem := new(big.Int)
	var out [26]byte
	for i := 25; i >= 0; i-- {
		n.DivMod(n, mod, rem)
		out[i] = enc[rem.Int64()]
	}
	return string(out[:])
}

// connectorConversion is the result of turning one metric-generating processor
// into a connector: the new connector node (to be appended to spec.connectors)
// and any per-field warnings produced while mapping the parameters.
type connectorConversion struct {
	id       string
	node     *yaml.Node
	warnings []string
}

// convertProcessorToConnector maps a count_telemetry / extract_metric_v2
// processor node to a count / signaltometrics connector node. The returned
// connector carries a fresh ULID id and empty routes (the caller wires routing).
func convertProcessorToConnector(proc *yaml.Node, connectorType, sourceLabel string) (*connectorConversion, error) {
	params := paramMap(proc)
	id := newULID()

	var (
		connParams []any
		warnings   []string
		err        error
	)
	switch connectorType {
	case "count":
		connParams, warnings, err = mapCountTelemetry(params, sourceLabel)
	case "signaltometrics":
		connParams, warnings, err = mapExtractMetric(params, sourceLabel)
	default:
		return nil, fmt.Errorf("no mapping for connector type %q", connectorType)
	}
	if err != nil {
		return nil, err
	}

	connector := map[string]any{
		"id":         id,
		"type":       connectorType, // bare type; server resolves the current version
		"parameters": connParams,
		"routes":     map[string]any{},
	}
	if dn := scalar(mapValue(proc, "displayName")); dn != "" {
		connector["displayName"] = dn
	}

	node := &yaml.Node{}
	if err := node.Encode(connector); err != nil {
		return nil, fmt.Errorf("encoding connector: %w", err)
	}
	return &connectorConversion{id: id, node: node, warnings: warnings}, nil
}

// mapCountTelemetry maps count_telemetry parameters to count connector
// parameters. count emits one custom metric per enabled signal type.
func mapCountTelemetry(params map[string]*yaml.Node, src string) ([]any, []string, error) {
	var warnings []string
	telemetry := decodeStringList(params["telemetry_types"])

	type sig struct {
		ttype, signalType, nameKey, matchKey, unitKey, enableAttrsKey, attrsKey string
	}
	signals := []sig{
		{"Logs", "logs", "log_metric_name", "log_match", "log_metric_units", "log_enable_attributes", "log_attributes"},
		{"Metrics", "datapoints", "datapoint_metric_name", "datapoint_match", "datapoint_metric_units", "datapoint_enable_attributes", "datapoint_attributes"},
		{"Traces", "spans", "span_metric_name", "span_match", "span_metric_units", "span_enable_attributes", "span_attributes"},
	}

	var metrics []any
	for _, s := range signals {
		if !containsStr(telemetry, s.ttype) {
			continue
		}
		name := decodeString(params[s.nameKey])
		if name == "" {
			continue
		}
		metric := map[string]any{
			"signalType":  s.signalType,
			"name":        name,
			"description": "",
			"condition":   normalizeCondition(params[s.matchKey]),
			"attributes":  []any{},
		}
		metrics = append(metrics, metric)

		// Lossy fields: units and interval have no home on the count connector,
		// and *_attributes were OTTL value expressions, not group-by keys.
		if u := decodeString(params[s.unitKey]); u != "" && u != "{logs}" && u != "{datapoints}" && u != "{spans}" {
			warnings = append(warnings, fmt.Sprintf("%s: dropped %s=%q (count connector has no units field)", src, s.unitKey, u))
		}
		if decodeBool(params[s.enableAttrsKey]) && hasMapEntries(params[s.attrsKey]) {
			warnings = append(warnings, fmt.Sprintf("%s: dropped %s (OTTL attribute expressions do not map to count group-by keys; review %q manually)", src, s.attrsKey, name))
		}
	}
	if interval := decodeString(params["interval"]); interval != "" && interval != "0" {
		warnings = append(warnings, fmt.Sprintf("%s: dropped interval=%s (count connector uses the pipeline interval)", src, interval))
	}
	if len(metrics) == 0 {
		warnings = append(warnings, src+": count_telemetry produced no metrics (no named metric for any enabled telemetry type)")
	}

	out := []any{
		map[string]any{"name": "telemetry_types", "value": telemetry},
		map[string]any{"name": "metrics", "value": metrics},
	}
	return out, warnings, nil
}

// mapExtractMetric maps extract_metric_v2 parameters to signaltometrics
// connector parameters. extract_metric_v2 produces metrics from log data, so
// every metric is signalType "logs". Several fields map only approximately and
// are flagged for review.
func mapExtractMetric(params map[string]*yaml.Node, src string) ([]any, []string, error) {
	var warnings []string

	var items []map[string]any
	if v := params["metrics"]; v != nil {
		_ = v.Decode(&items)
	}

	var metrics []any
	for _, it := range items {
		name, _ := it["metricName"].(string)
		if name == "" {
			continue
		}
		unit, _ := it["metricUnit"].(string)
		metricType, _ := it["metricType"].(string)
		field, _ := it["metricField"].(string)
		match, _ := it["match"].(string)

		sigType, value := mapExtractType(metricType, match, field)

		metric := map[string]any{
			"signalType":         "logs",
			"name":               name,
			"description":        "",
			"unit":               unit,
			"type":               sigType,
			"value":              value,
			"condition":          normalizeConditionValue(it["condition"]),
			"attributes":         []any{},
			"resourceAttributes": []any{},
			"buckets":            []any{},
			"count":              "",
		}
		metrics = append(metrics, metric)

		warnings = append(warnings,
			fmt.Sprintf("%s: signaltometrics metric %q: type=%q and value=%q are best-effort from metricType=%q/metricField=%q — verify before rollout",
				src, name, sigType, value, metricType, field))
		if hasMapEntriesValue(it["metricAttributes"]) {
			warnings = append(warnings, fmt.Sprintf("%s: signaltometrics metric %q: metricAttributes not carried over (review group-by keys manually)", src, name))
		}
	}
	if len(metrics) == 0 {
		warnings = append(warnings, src+": extract_metric_v2 produced no metrics")
	}

	out := []any{
		map[string]any{"name": "telemetry_types", "value": []string{"Logs"}},
		map[string]any{"name": "metrics", "value": metrics},
	}
	return out, warnings, nil
}

// mapExtractType approximates the signaltometrics metric type and value
// expression from extract_metric_v2's metricType, match location, and field.
func mapExtractType(metricType, match, field string) (sigType, value string) {
	// Build an OTTL value expression from where the field lives.
	switch match {
	case "Body":
		value = fmt.Sprintf("body[%q]", field)
	case "Attribute", "Attributes":
		value = fmt.Sprintf("attributes[%q]", field)
	case "Resource":
		value = fmt.Sprintf("resource.attributes[%q]", field)
	default:
		if field != "" {
			value = fmt.Sprintf("attributes[%q]", field)
		} else {
			value = "1"
		}
	}
	switch {
	case containsFold(metricType, "gauge"):
		sigType = "gauge"
	case containsFold(metricType, "histogram"):
		sigType = "histogram"
	default:
		sigType = "sum"
	}
	return sigType, value
}

// --- parameter decoding helpers ---

// paramMap returns a map of parameter name -> value node for a processor's
// "parameters" list ([{name, value}, ...]).
func paramMap(proc *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	seq := mapValue(proc, "parameters")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return out
	}
	for _, p := range seq.Content {
		name := scalar(mapValue(p, "name"))
		if name != "" {
			out[name] = mapValue(p, "value")
		}
	}
	return out
}

func decodeString(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	var s string
	if err := n.Decode(&s); err != nil {
		return ""
	}
	return s
}

func decodeBool(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	var b bool
	_ = n.Decode(&b)
	return b
}

func decodeStringList(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	var s []string
	_ = n.Decode(&s)
	return s
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + ('a' - 'A')
		}
		return b
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// hasMapEntries reports whether a node is a non-empty mapping.
func hasMapEntries(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.MappingNode && len(n.Content) > 0
}

func hasMapEntriesValue(v any) bool {
	m, ok := v.(map[string]any)
	return ok && len(m) > 0
}

// normalizeCondition turns a count_telemetry *_match value node into a count
// connector condition value. An empty/string match becomes an empty condition;
// an object match is carried across verbatim.
func normalizeCondition(n *yaml.Node) any {
	if n != nil && n.Kind == yaml.MappingNode {
		var m map[string]any
		if err := n.Decode(&m); err == nil && len(m) > 0 {
			return m
		}
	}
	return emptyCondition()
}

func normalizeConditionValue(v any) any {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		return m
	}
	return emptyCondition()
}

func emptyCondition() map[string]any {
	return map[string]any{
		"ottl": "",
		"ui":   map[string]any{"operator": "", "statements": []any{}},
	}
}
