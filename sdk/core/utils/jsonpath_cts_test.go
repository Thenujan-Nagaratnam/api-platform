package utils

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestJSONPathQuery_ComplianceSuite runs the official RFC 9535 compliance test
// suite (https://github.com/jsonpath-standard/jsonpath-compliance-test-suite).
// Set JSONPATH_CTS to the path of its cts.json; without it the test is skipped.
func TestJSONPathQuery_ComplianceSuite(t *testing.T) {
	path := os.Getenv("JSONPATH_CTS")
	if path == "" {
		t.Skip("JSONPATH_CTS is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		Tests []struct {
			Name            string          `json:"name"`
			Selector        string          `json:"selector"`
			Document        json.RawMessage `json:"document"`
			Result          []any           `json:"result"`
			Results         [][]any         `json:"results"`
			ResultPaths     []string        `json:"result_paths"`
			ResultsPaths    [][]string      `json:"results_paths"`
			InvalidSelector bool            `json:"invalid_selector"`
		} `json:"tests"`
	}
	if err := json.Unmarshal(raw, &suite); err != nil {
		t.Fatal(err)
	}
	pass := 0
	for _, tc := range suite.Tests {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			q, err := CompileJSONPath(tc.Selector)
			if tc.InvalidSelector {
				if err == nil {
					t.Errorf("%q should be rejected", tc.Selector)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q rejected: %v", tc.Selector, err)
			}
			var doc any
			if len(tc.Document) > 0 {
				if err := json.Unmarshal(tc.Document, &doc); err != nil {
					t.Fatal(err)
				}
			}
			nodes := q.Select(doc)
			vals := make([]any, len(nodes))
			paths := make([]string, len(nodes))
			for i, n := range nodes {
				vals[i], paths[i] = n.Value, n.Path
			}
			okVals := false
			if tc.Result != nil || tc.Results == nil {
				okVals = deepEq(vals, tc.Result)
			}
			for _, r := range tc.Results {
				okVals = okVals || deepEq(vals, r)
			}
			if !okVals {
				t.Errorf("%q\n got %v\nwant %v %v", tc.Selector, vals, tc.Result, tc.Results)
			}
			okPaths := tc.ResultPaths == nil && tc.ResultsPaths == nil
			if tc.ResultPaths != nil {
				okPaths = reflect.DeepEqual(paths, tc.ResultPaths)
			}
			for _, r := range tc.ResultsPaths {
				okPaths = okPaths || reflect.DeepEqual(paths, r)
			}
			if !okPaths {
				t.Errorf("%q\n got paths %v\nwant %v %v", tc.Selector, paths, tc.ResultPaths, tc.ResultsPaths)
			}
			if okVals && okPaths {
				pass++
			}
		})
	}
	t.Logf("%d of %d compliance cases pass", pass, len(suite.Tests))
}

func deepEq(a, b []any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
