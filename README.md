# v1converter

Convert [Bindplane](https://bindplane.com) Configuration YAML from `apiVersion`
**v1** to **v2**, handling the processors that no longer exist as processors in
v2:

| v1 processor        | v2 result                                   |
| ------------------- | ------------------------------------------- |
| `health_check`      | moved to a `spec.extensions` entry          |
| `pprof`             | moved to a `spec.extensions` entry          |
| `count_telemetry`   | converted to a **`count` connector**        |
| `extract_metric_v2` | converted to a **`signaltometrics` connector** |
| `extract_metric` (deprecated) | no clean mapping — config skipped and reported |

It operates on **local YAML files** and preserves key order and untouched fields
verbatim, so anything outside the conversion comes through unchanged. A
configuration it can't fully convert is reported and skipped, never written
half-converted.

## Build

```bash
go build -o v1converter .
```

Requires Go 1.26+. The only dependency is `gopkg.in/yaml.v3`.

## Usage

```bash
v1converter [flags] <file.yaml>...
```

| Flag         | Effect                                                          |
| ------------ | -------------------------------------------------------------- |
| `--dry-run`  | Report what would change without writing any files.            |
| `-v`         | List every change and warning per configuration.               |
| `--out-dir`  | Write converted files into a directory (same filenames).       |
| `--in-place` | Overwrite each input file with its converted output.           |

Default output is `<name>.v2.yaml` next to each input. Exit code is non-zero if
any configuration was skipped or errored.

## Recommended workflow

Export with the `bindplane` CLI, convert, review, then apply:

```bash
# 1. Export the v1 config(s) you want to convert.
bindplane get configuration my-config -o yaml > my-config.yaml

# 2. Convert. Always dry-run first and read the warnings.
v1converter --dry-run -v my-config.yaml

# 3. Write the converted file.
v1converter my-config.yaml

# 4. Review my-config.v2.yaml (especially anything flagged ⚠), then apply.
bindplane apply my-config.v2.yaml
```

See [`examples/example.v1.yaml`](examples/example.v1.yaml) for a self-contained
config you can convert and apply with no external dependencies.

## What the connector conversion does

For each `count_telemetry` / `extract_metric_v2` processor:

1. A new top-level connector is created in `spec.connectors` with a fresh ID and
   the processor's parameters mapped to the connector's schema.
2. The processor is removed from its source or destination.
3. **Routing is rebuilt for the whole config.** Connectors must be wired
   explicitly, and Bindplane treats a config's routing as all-or-nothing, so
   when any route is defined every source needs routes. The tool gives each
   source a `logs+metrics+traces` route to all destinations (matching
   Bindplane's default routing), appends the converted connector to the route of
   the source it came from, and routes each connector's generated metrics to all
   destinations (preserving v1's behavior where generated metrics flowed to
   every destination). Connectors self-filter by their `telemetry_types`, so a
   combined route is safe.

When a config has no metric processors (only a `health_check`/`pprof` move, or
nothing), routing is left empty and Bindplane connects sources to destinations
automatically on apply.

## Review before rollout (⚠ warnings)

The conversion is not always 1:1. The tool maps what maps and warns on the rest
rather than failing or silently dropping:

- **`count`** has no `units` or `interval` fields — both are dropped (the
  connector counts on the pipeline interval).
- **`count_telemetry` attributes** were value expressions; the `count`
  connector's attributes are group-by keys. These do not map and are dropped
  with a warning to review manually.
- **`extract_metric_v2` → `signaltometrics`** `type` and `value` are best-effort
  (for example `metricType: gauge_int` → `type: gauge`, and `match: Body` +
  `metricField: dropped_items` → `value: body["dropped_items"]`). Verify these
  before rollout.

Routing to all destinations and these mappings are deliberate defaults; review
the converted file and adjust before applying.

## Notes and limitations

- **Inline processors only.** Processors referenced by name (`{id, name: Foo:6}`)
  point at a saved Processor resource whose type can't be determined from the
  config file alone, so they are left untouched.
- **Telemetry-type compatibility is approximate.** The tool routes every
  source's combined pipeline to all destinations, matching Bindplane's default
  routing behavior.
- **Destination-attached metric processors** are converted but flagged for
  manual placement review.

## Tests

```bash
go test ./...
```
