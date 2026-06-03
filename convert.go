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
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	apiV1 = "bindplane.observiq.com/v1"
	apiV2 = "bindplane.observiq.com/v2"
)

// ruleAction describes what the converter does with an affected processor type.
type ruleAction int

const (
	// actionMoveToExtension removes the processor from its component's processor
	// list and appends an equivalent entry to spec.extensions, dropping the
	// version suffix so the server resolves the current extension-type version.
	actionMoveToExtension ruleAction = iota

	// actionToConnector turns a metric-generating processor into a top-level
	// connector (spec.connectors) and wires it into the routing graph.
	actionToConnector

	// actionPending marks a processor type as known-affected but without an
	// implemented mapping. Encountering one fails loud: the whole configuration
	// is skipped and reported, never half-converted.
	actionPending
)

type processorRule struct {
	action        ruleAction
	connectorType string // for actionToConnector: "count" or "signaltometrics"
	note          string
}

// processorRules maps a processor base type (the part before ":version") to its
// v1->v2 conversion rule.
//
//   - health_check / pprof became extension-only types -> move to spec.extensions.
//   - count_telemetry / extract_metric_v2 are metric-generating processors that
//     became connectors -> count / signaltometrics, wired through routing.
//   - extract_metric (the deprecated v1, non-_v2 one) has a different parameter
//     schema and is left pending so it can never silently mis-convert.
var processorRules = map[string]processorRule{
	"health_check": {action: actionMoveToExtension, note: "health_check is an extension in v2"},
	"pprof":        {action: actionMoveToExtension, note: "pprof is an extension in v2"},

	"count_telemetry":   {action: actionToConnector, connectorType: "count"},
	"extract_metric_v2": {action: actionToConnector, connectorType: "signaltometrics"},

	"extract_metric": {action: actionPending, note: "deprecated v1 extract_metric; use extract_metric_v2 or map by hand"},
}

// ConfigResult is the outcome of converting one Configuration document.
type ConfigResult struct {
	Name       string
	Applicable bool
	Converted  bool
	Changes    []string
	Warnings   []string
	SkipReason string // non-empty => skipped (fail-loud); no mutation performed
}

type component struct {
	node  *yaml.Node
	label string
}

// convertDocument converts a single YAML document node (a DocumentNode) in
// place. It is two-pass: it first scans for any pending (unmappable) processor
// and, if found, returns a SkipReason without mutating anything; only a fully
// convertible config is mutated, so a config is never written half-converted.
func convertDocument(doc *yaml.Node) (*ConfigResult, error) {
	root := documentRoot(doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return &ConfigResult{}, nil
	}
	if scalar(mapValue(root, "kind")) != "Configuration" {
		return &ConfigResult{}, nil
	}
	apiVersionNode := mapValue(root, "apiVersion")
	if scalar(apiVersionNode) != apiV1 {
		return &ConfigResult{}, nil
	}

	spec := mapValue(root, "spec")
	res := &ConfigResult{Name: configName(root), Applicable: true}
	if spec == nil || spec.Kind != yaml.MappingNode {
		apiVersionNode.Value = apiV2
		res.Converted = true
		res.Changes = append(res.Changes, "bumped apiVersion to v2 (no spec)")
		return res, nil
	}

	components := collectComponents(spec)

	// Pass 1: scan for pending processors. Any hit => skip the whole config.
	var pending []string
	for _, c := range components {
		for _, proc := range componentProcessors(c.node) {
			if base, ok := processorBaseType(proc); ok {
				if rule, found := processorRules[base]; found && rule.action == actionPending {
					pending = append(pending, fmt.Sprintf("%s in %s (%s)", base, c.label, rule.note))
				}
			}
		}
	}
	if len(pending) > 0 {
		res.SkipReason = "contains processor(s) with no v2 mapping: " + strings.Join(pending, "; ")
		return res, nil
	}

	// Pass 2: perform extension moves and connector conversions.
	// attachments records, per source/component node, the connector IDs created
	// from processors on it, so routing can wire them to the right pipeline.
	attachments := map[*yaml.Node][]string{}
	var anyConnector bool

	for _, c := range components {
		procSeq := componentProcessorSeq(c.node)
		if procSeq == nil {
			continue
		}
		kept := procSeq.Content[:0:0]
		for _, proc := range procSeq.Content {
			base, ok := processorBaseType(proc)
			if !ok {
				kept = append(kept, proc)
				continue
			}
			rule, found := processorRules[base]
			if !found {
				kept = append(kept, proc)
				continue
			}
			switch rule.action {
			case actionMoveToExtension:
				moveProcessorToExtension(spec, proc, base)
				res.Changes = append(res.Changes,
					fmt.Sprintf("moved %q (%s) from %s to spec.extensions", displayName(proc), base, c.label))
			case actionToConnector:
				conv, err := convertProcessorToConnector(proc, rule.connectorType, c.label)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", c.label, err)
				}
				appendToSeq(spec, "connectors", conv.node)
				attachments[c.node] = append(attachments[c.node], conv.id)
				res.Changes = append(res.Changes,
					fmt.Sprintf("converted %q (%s) on %s to %s connector %s",
						displayName(proc), base, c.label, rule.connectorType, conv.id))
				res.Warnings = append(res.Warnings, conv.warnings...)
				anyConnector = true
			default:
				kept = append(kept, proc)
			}
		}
		procSeq.Content = kept
	}

	// Routing only needs to be rebuilt when we created connectors: connectors
	// must be wired explicitly into the pipeline, and because Bindplane treats a
	// config's routing as all-or-nothing, defining any route means we must
	// define routes for every source. When we only moved extensions (or did
	// nothing), routes are left empty so Bindplane connects sources to
	// destinations automatically on apply.
	if anyConnector {
		if err := buildRoutes(spec, attachments, res); err != nil {
			return nil, err
		}
	}

	apiVersionNode.Value = apiV2
	res.Converted = true
	res.Changes = append(res.Changes, "bumped apiVersion to v2")
	return res, nil
}

// buildRoutes constructs the v2 routing graph: a single logs+metrics+traces
// route to all destinations for every source (matching Bindplane's default
// routing), additionally appending any connectors attached to that source. Each
// connector routes its generated metrics to all destinations, carrying v1's
// "metrics go everywhere" behavior. Connectors self-filter by their
// telemetry_types, so a combined route is safe.
func buildRoutes(spec *yaml.Node, attachments map[*yaml.Node][]string, res *ConfigResult) error {
	ensureIDs(spec, "sources", "s-")
	ensureIDs(spec, "destinations", "d-")

	destPaths := componentPaths(spec, "destinations")
	if len(destPaths) == 0 {
		res.Warnings = append(res.Warnings, "no destinations: connectors created but not routed to any destination")
	}

	sources := mapValue(spec, "sources")
	if sources != nil && sources.Kind == yaml.SequenceNode {
		for _, src := range sources.Content {
			comps := append([]string{}, destPaths...)
			for _, cid := range attachments[src] {
				comps = append(comps, "connectors/"+cid)
			}
			routes := map[string]any{
				"logs+metrics+traces": []any{
					map[string]any{"id": "0", "components": comps},
				},
			}
			if err := setMapNode(src, "routes", routes); err != nil {
				return err
			}
		}
	}

	// Any connector attached to a destination cannot be expressed as a simple
	// source route; flag it for manual placement, then still wire its output.
	for node, ids := range attachments {
		if scalar(mapValue(node, "name")) != "" && isDestination(spec, node) {
			for _, id := range ids {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("connector %s came from a destination-attached processor; verify its placement in routing manually", id))
			}
		}
	}

	// Route every created connector's metrics to all destinations.
	connectors := mapValue(spec, "connectors")
	if connectors != nil && connectors.Kind == yaml.SequenceNode {
		for _, conn := range connectors.Content {
			routes := map[string]any{
				"metrics": []any{
					map[string]any{"id": "0", "components": append([]string{}, destPaths...)},
				},
			}
			if err := setMapNode(conn, "routes", routes); err != nil {
				return err
			}
		}
	}
	return nil
}

func isDestination(spec, node *yaml.Node) bool {
	dests := mapValue(spec, "destinations")
	if dests == nil {
		return false
	}
	for _, d := range dests.Content {
		if d == node {
			return true
		}
	}
	return false
}

// moveProcessorToExtension rewrites the processor's type to the bare base and
// appends it to spec.extensions (creating the list if needed). The node pointer
// is reused; the caller drops it from the processor list.
func moveProcessorToExtension(spec, proc *yaml.Node, base string) {
	if t := mapValue(proc, "type"); t != nil {
		t.Value = base
		t.Tag = "!!str"
		t.Style = 0
	}
	appendToSeq(spec, "extensions", proc)
}

func collectComponents(spec *yaml.Node) []component {
	var out []component
	for _, key := range []string{"sources", "destinations", "connectors", "processors"} {
		seq := mapValue(spec, key)
		if seq == nil || seq.Kind != yaml.SequenceNode {
			continue
		}
		for i, item := range seq.Content {
			if item.Kind != yaml.MappingNode {
				continue
			}
			out = append(out, component{node: item, label: fmt.Sprintf("%s[%d] (%s)", key, i, componentRef(item))})
		}
	}
	return out
}

func componentProcessorSeq(comp *yaml.Node) *yaml.Node {
	seq := mapValue(comp, "processors")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	return seq
}

func componentProcessors(comp *yaml.Node) []*yaml.Node {
	if seq := componentProcessorSeq(comp); seq != nil {
		return seq.Content
	}
	return nil
}

func processorBaseType(proc *yaml.Node) (string, bool) {
	t := mapValue(proc, "type")
	if t == nil || t.Kind != yaml.ScalarNode || t.Value == "" {
		return "", false
	}
	return baseName(t.Value), true
}

func baseName(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i]
	}
	return s
}

// --- yaml.Node helpers ---

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// appendToSeq appends value to the sequence stored at key in mapping m, creating
// the sequence if absent.
func appendToSeq(m *yaml.Node, key string, value *yaml.Node) {
	seq := mapValue(m, key)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		mapSet(m, key, seq)
	}
	seq.Content = append(seq.Content, value)
}

// setMapNode encodes a Go value and sets it at key in mapping m.
func setMapNode(m *yaml.Node, key string, value any) error {
	n := &yaml.Node{}
	if err := n.Encode(value); err != nil {
		return err
	}
	mapSet(m, key, n)
	return nil
}

// ensureIDs gives every entry in the named sequence an id, deriving one from its
// name (prefix + sanitized name) when missing.
func ensureIDs(spec *yaml.Node, key, prefix string) {
	seq := mapValue(spec, key)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return
	}
	for _, item := range seq.Content {
		if scalar(mapValue(item, "id")) != "" {
			continue
		}
		id := newULID()
		if name := scalar(mapValue(item, "name")); name != "" {
			id = prefix + baseName(name)
		}
		mapSet(item, "id", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: id})
	}
}

// componentPaths returns "<key>/<id>" paths for every entry in the named
// sequence (e.g. "destinations/d-foo").
func componentPaths(spec *yaml.Node, key string) []string {
	seq := mapValue(spec, key)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	var paths []string
	for _, item := range seq.Content {
		if id := scalar(mapValue(item, "id")); id != "" {
			paths = append(paths, key+"/"+id)
		}
	}
	return paths
}

func scalar(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}

func configName(root *yaml.Node) string {
	return scalar(mapValue(mapValue(root, "metadata"), "name"))
}

func componentRef(comp *yaml.Node) string {
	if v := scalar(mapValue(comp, "name")); v != "" {
		return v
	}
	if v := scalar(mapValue(comp, "displayName")); v != "" {
		return v
	}
	return scalar(mapValue(comp, "type"))
}

func displayName(proc *yaml.Node) string {
	if v := scalar(mapValue(proc, "displayName")); v != "" {
		return v
	}
	if v := scalar(mapValue(proc, "name")); v != "" {
		return v
	}
	return scalar(mapValue(proc, "type"))
}
