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
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func parseDoc(t *testing.T, s string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &doc
}

func reencode(t *testing.T, doc *yaml.Node) string {
	t.Helper()
	out, err := encodeDocuments([]*yaml.Node{doc})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(out)
}

// health_check on a source moves into spec.extensions, version suffix stripped.
func TestMoveHealthCheckToExtension(t *testing.T) {
	doc := parseDoc(t, `
apiVersion: bindplane.observiq.com/v1
kind: Configuration
metadata:
    name: cfg
spec:
    sources:
        - id: s1
          type: journald:1
          processors:
            - id: p1
              displayName: Agent Health Check
              type: health_check:6
              parameters:
                - name: listen_port
                  value: 8080
    destinations:
        - name: googlecloud
`)
	res, err := convertDocument(doc)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.SkipReason != "" {
		t.Fatalf("unexpected skip: %s", res.SkipReason)
	}
	out := reencode(t, doc)
	if !strings.Contains(out, "bindplane.observiq.com/v2") {
		t.Errorf("apiVersion not bumped:\n%s", out)
	}
	if strings.Contains(out, "health_check:6") {
		t.Errorf("version suffix not stripped:\n%s", out)
	}
	if !strings.Contains(out, "extensions:") {
		t.Errorf("extensions block not created:\n%s", out)
	}
	src := out[strings.Index(out, "sources:"):strings.Index(out, "destinations:")]
	if strings.Contains(src, "health_check") {
		t.Errorf("health_check still under sources:\n%s", src)
	}
	// No connectors created => routing left to the server (no routes injected).
	if strings.Contains(out, "connectors:") {
		t.Errorf("unexpected connectors block:\n%s", out)
	}
}

// count_telemetry becomes a count connector, is removed from the source, and the
// source's route gains the connector while the connector routes metrics out.
func TestCountTelemetryToConnector(t *testing.T) {
	doc := parseDoc(t, `
apiVersion: bindplane.observiq.com/v1
kind: Configuration
metadata:
    name: cfg
spec:
    sources:
        - id: 01SRC
          type: tcp:5
          processors:
            - id: p1
              displayName: Logs Sent Metric
              type: count_telemetry:4
              parameters:
                - name: telemetry_types
                  value: [Logs]
                - name: log_match
                  value: ""
                - name: log_metric_name
                  value: custom.logs.sent
                - name: log_metric_units
                  value: '{logs}'
                - name: log_enable_attributes
                  value: true
                - name: log_attributes
                  value:
                    metric_log_type: attributes["chronicle_log_type"]
                - name: interval
                  value: 60
          routes: {}
    destinations:
        - id: d-gcp
          name: gcp:6
`)
	res, err := convertDocument(doc)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.SkipReason != "" {
		t.Fatalf("unexpected skip: %s", res.SkipReason)
	}
	out := reencode(t, doc)

	if !strings.Contains(out, "connectors:") {
		t.Fatalf("no connectors block:\n%s", out)
	}
	if !strings.Contains(out, "type: count") {
		t.Errorf("count connector type missing:\n%s", out)
	}
	if !strings.Contains(out, "custom.logs.sent") {
		t.Errorf("metric name not carried into connector:\n%s", out)
	}
	// Processor removed from the source.
	src := out[strings.Index(out, "sources:"):strings.Index(out, "connectors:")]
	if strings.Contains(src, "count_telemetry") {
		t.Errorf("count_telemetry still under sources:\n%s", src)
	}
	// Source route references the connector and the destination.
	if !strings.Contains(out, "connectors/") || !strings.Contains(out, "destinations/d-gcp") {
		t.Errorf("source route not wired:\n%s", out)
	}
	// Lossy-field warnings: units, interval, attributes.
	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"interval", "attribute"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected warning containing %q, got:\n%s", want, joined)
		}
	}
}

// extract_metric_v2 becomes a signaltometrics connector with a best-effort
// value expression and a review warning.
func TestExtractMetricToSignalToMetrics(t *testing.T) {
	doc := parseDoc(t, `
apiVersion: bindplane.observiq.com/v1
kind: Configuration
metadata:
    name: cfg
spec:
    sources:
        - id: 01SRC
          type: otlp:5
          processors:
            - id: p1
              displayName: Dropped Log Metrics
              type: extract_metric_v2:4
              parameters:
                - name: metrics
                  value:
                    - match: Body
                      metricField: dropped_items
                      metricName: custom.logs.dropped
                      metricType: gauge_int
                      metricUnit: '{logs}'
                      metricAttributes: {}
                      condition:
                        ottl: ""
    destinations:
        - id: d-gcp
          name: gcp:6
`)
	res, err := convertDocument(doc)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	out := reencode(t, doc)
	if !strings.Contains(out, "type: signaltometrics") {
		t.Fatalf("signaltometrics connector missing:\n%s", out)
	}
	if !strings.Contains(out, "custom.logs.dropped") {
		t.Errorf("metric name missing:\n%s", out)
	}
	if !strings.Contains(out, `body["dropped_items"]`) {
		t.Errorf("value expression not derived from match/field:\n%s", out)
	}
	if len(res.Warnings) == 0 {
		t.Error("expected best-effort warnings for signaltometrics mapping")
	}
}

// The deprecated v1 extract_metric has no clean mapping and fails loud.
func TestExtractMetricV1Skipped(t *testing.T) {
	doc := parseDoc(t, `
apiVersion: bindplane.observiq.com/v1
kind: Configuration
metadata:
    name: cfg
spec:
    sources:
        - id: s1
          type: otlp:5
          processors:
            - id: p1
              type: extract_metric:2
              parameters: []
`)
	res, err := convertDocument(doc)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.SkipReason == "" {
		t.Fatal("expected SkipReason for deprecated extract_metric")
	}
	if res.Converted {
		t.Fatal("skipped config must not be Converted")
	}
}

func TestNonConfigurationIgnored(t *testing.T) {
	doc := parseDoc(t, "apiVersion: bindplane.observiq.com/v1\nkind: Source\nmetadata:\n  name: x\n")
	res, err := convertDocument(doc)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.Applicable {
		t.Fatal("Source should not be applicable")
	}
}
