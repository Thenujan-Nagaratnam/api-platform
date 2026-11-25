/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.org) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

package utils

import (
	"reflect"
	"sort"
	"testing"
)

const rfcDoc = `{
 "system": "sys",
 "tools": [{"name": "t1", "description": "d1", "input_schema": {"maximum": 9007199254740991}}],
 "messages": [
  {"role": "user", "content": "hello"},
  {"role": "assistant", "content": [{"type": "text", "text": "a1"}]},
  {"role": "user", "content": [
    {"type": "text", "text": "u1"},
    {"type": "tool_result", "content": [{"type": "text", "text": "r1"}]}
  ]}
 ],
 "it's": {"a\\b": 1}
}`

func TestSelectNormalizedPaths(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want []string
	}{
		{"root", "$", []string{"$"}},
		{"name", "$.system", []string{"$['system']"}},
		{"bracket name", "$['system']", []string{"$['system']"}},
		{"index", "$.messages[0].content", []string{"$['messages'][0]['content']"}},
		{"negative index", "$.messages[-1].content[0].text", []string{"$['messages'][2]['content'][0]['text']"}},
		{"default legacy path", "$.messages[-1].content", []string{"$['messages'][2]['content']"}},
		{"wildcard", "$.messages[*].role", []string{"$['messages'][0]['role']", "$['messages'][1]['role']", "$['messages'][2]['role']"}},
		{"slice", "$.messages[0:2].role", []string{"$['messages'][0]['role']", "$['messages'][1]['role']"}},
		{"slice reverse", "$.messages[::-1].role", []string{"$['messages'][2]['role']", "$['messages'][1]['role']", "$['messages'][0]['role']"}},
		{"union", "$.messages[0,2].role", []string{"$['messages'][0]['role']", "$['messages'][2]['role']"}},
		{"filter eq", "$.messages[?(@.role=='user')].role", []string{"$['messages'][0]['role']", "$['messages'][2]['role']"}},
		{"filter and", "$.messages[?(@.role=='user' && @.content)].role", []string{"$['messages'][0]['role']", "$['messages'][2]['role']"}},
		{"filter not", "$.messages[?(!(@.role=='user'))].role", []string{"$['messages'][1]['role']"}},
		{"filter or", "$.messages[?(@.role=='user' || @.role=='assistant')].role", []string{"$['messages'][0]['role']", "$['messages'][1]['role']", "$['messages'][2]['role']"}},
		{"filter nested", "$.messages[?(@.role=='user')].content[?(@.type=='text')].text", []string{"$['messages'][2]['content'][0]['text']"}},
		{"filter number", "$.tools[?(@.input_schema.maximum > 5)].name", []string{"$['tools'][0]['name']"}},
		{"filter function", "$.messages[?(match(@.role, 'us.r'))].role", []string{"$['messages'][0]['role']", "$['messages'][2]['role']"}},
		{"filter length", "$.messages[?(length(@.content) == 2)].role", []string{"$['messages'][2]['role']"}},
		{"descendant name", "$.messages[?(@.role=='user')]..text", []string{"$['messages'][2]['content'][0]['text']", "$['messages'][2]['content'][1]['content'][0]['text']"}},
		{"descendant union", "$.messages[?(@.role=='user')]..['text','content']", nil},
		{"no match", "$.nothing", nil},
		{"escaped name", `$["it's"]["a\\b"]`, []string{`$['it\'s']['a\\b']`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SelectNormalizedPaths([]byte(rfcDoc), tt.expr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.name == "descendant union" {
				// Content strings, arrays and text strings under user messages.
				want := []string{
					"$['messages'][0]['content']",
					"$['messages'][2]['content']",
					"$['messages'][2]['content'][0]['text']",
					"$['messages'][2]['content'][1]['content']",
					"$['messages'][2]['content'][1]['content'][0]['text']",
				}
				sort.Strings(got)
				sort.Strings(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("got %v want %v", got, want)
				}
				return
			}
			if tt.want == nil {
				if len(got) != 0 {
					t.Fatalf("expected no matches, got %v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestSelectNormalizedPathsErrors(t *testing.T) {
	for _, expr := range []string{"", "messages", "$.", "$[", "$[?", "$.a[?(@.b ==)]", "$.a[?foo(@.b)]", "$..", "$['a'"} {
		if _, err := SelectNormalizedPaths([]byte(`{"a":1}`), expr); err == nil {
			t.Errorf("expected an error for %q", expr)
		}
	}
	if _, err := SelectNormalizedPaths([]byte(`{bad`), "$"); err == nil {
		t.Error("expected an error for an invalid document")
	}
}

func TestSelectNormalizedPathsDoesNotRewriteNumbers(t *testing.T) {
	// The evaluator is read-only; it must accept numbers that do not fit a float64 exactly.
	got, err := SelectNormalizedPaths([]byte(`{"n": 9007199254740993, "s": "x"}`), "$.s")
	if err != nil || len(got) != 1 || got[0] != "$['s']" {
		t.Fatalf("got %v err %v", got, err)
	}
}
