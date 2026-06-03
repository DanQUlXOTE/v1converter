// Command bp-v1-to-v2 converts Bindplane Configuration YAML from apiVersion v1
// to v2, moving processors that are no longer available as processors (because
// they became extensions) into the spec.extensions list.
//
// It operates on local YAML files only. A configuration that contains an
// affected processor with no implemented v2 mapping is reported and skipped
// (never written half-converted).
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	outDir := flag.String("out-dir", "", "directory to write converted files into (default: alongside input as <name>.v2.yaml)")
	inPlace := flag.Bool("in-place", false, "overwrite each input file with its converted output")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing any files")
	verbose := flag.Bool("v", false, "verbose: list every change per configuration")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bp-v1-to-v2 [flags] <file.yaml>...")
		flag.PrintDefaults()
	}
	flag.Parse()

	files := flag.Args()
	if len(files) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if *inPlace && *outDir != "" {
		fmt.Fprintln(os.Stderr, "error: --in-place and --out-dir are mutually exclusive")
		os.Exit(2)
	}

	var converted, skipped, failed, unchanged int
	for _, path := range files {
		switch err := processFile(path, *outDir, *inPlace, *dryRun, *verbose, &converted, &skipped, &unchanged); {
		case err != nil:
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", path, err)
			failed++
		}
	}

	fmt.Printf("\n%d converted, %d skipped (need mapping), %d unchanged, %d errored\n",
		converted, skipped, unchanged, failed)
	if skipped > 0 || failed > 0 {
		os.Exit(1)
	}
}

// processFile reads one YAML file (which may hold multiple documents), converts
// every v1 Configuration in it, and writes the result. If any configuration in
// the file must be skipped, the file is left unwritten (fail loud, skip).
func processFile(path, outDir string, inPlace, dryRun, verbose bool, converted, skipped, unchanged *int) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	docs, err := decodeDocuments(raw)
	if err != nil {
		return fmt.Errorf("parsing YAML: %w", err)
	}

	var (
		results   []*ConfigResult
		anyChange bool
		anySkip   bool
	)
	for _, doc := range docs {
		res, err := convertDocument(doc)
		if err != nil {
			return err
		}
		if !res.Applicable {
			continue
		}
		results = append(results, res)
		if res.SkipReason != "" {
			anySkip = true
		}
		if res.Converted {
			anyChange = true
		}
	}

	if len(results) == 0 {
		if verbose {
			fmt.Printf("- %s: no v1 configurations\n", path)
		}
		*unchanged++
		return nil
	}

	// Fail loud: if any configuration in the file needs a mapping we do not have,
	// do not write the file at all. Report each skipped config.
	if anySkip {
		for _, r := range results {
			if r.SkipReason != "" {
				fmt.Fprintf(os.Stderr, "✗ %s [%s]: %s\n", path, r.Name, r.SkipReason)
				*skipped++
			}
		}
		return nil
	}

	if !anyChange {
		if verbose {
			fmt.Printf("- %s: already v2 / nothing to do\n", path)
		}
		*unchanged++
		return nil
	}

	for _, r := range results {
		fmt.Printf("✓ %s [%s]: converted to v2\n", path, r.Name)
		*converted++
		if verbose {
			for _, ch := range r.Changes {
				fmt.Printf("    - %s\n", ch)
			}
		}
		for _, w := range r.Warnings {
			fmt.Printf("    ⚠ %s\n", w)
		}
	}

	if dryRun {
		return nil
	}

	out, err := encodeDocuments(docs)
	if err != nil {
		return fmt.Errorf("re-encoding YAML: %w", err)
	}
	target := outputPath(path, outDir, inPlace)
	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(target, out, 0o644); err != nil {
		return err
	}
	if target != path {
		fmt.Printf("    wrote %s\n", target)
	}
	return nil
}

// decodeDocuments parses every YAML document in raw into a slice of node trees,
// preserving key order and unknown fields verbatim.
func decodeDocuments(raw []byte) ([]*yaml.Node, error) {
	var docs []*yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, &doc)
	}
	return docs, nil
}

// encodeDocuments serializes the documents back to YAML using 4-space indent to
// match Bindplane's output style.
func encodeDocuments(docs []*yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// outputPath decides where to write the converted file.
func outputPath(path, outDir string, inPlace bool) string {
	if inPlace {
		return path
	}
	if outDir != "" {
		return filepath.Join(outDir, filepath.Base(path))
	}
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + ".v2" + ext
}
