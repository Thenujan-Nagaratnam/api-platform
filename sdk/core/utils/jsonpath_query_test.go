/*
 * Copyright (c) 2026, WSO2 LLC. (http://www.wso2.org) All Rights Reserved.
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package utils

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func decodeDoc(t testing.TB, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

const chatDoc = `{
  "model": "claude",
  "system": "be brief",
  "messages": [
    {"role": "user", "content": "first"},
    {"role": "assistant", "content": [{"type": "text", "text": "ok"}]},
    {"role": "user", "content": [{"type": "tool_result", "content": "found"}]},
    {"role": "tool", "content": "t"},
    {"role": "user", "content": "last"}
  ]
}`

func values(t *testing.T, doc any, path string) []any {
	t.Helper()
	v, err := QueryJSONPathValues(doc, path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

// The acceptance criteria of wso2/api-platform#3571.
func TestJSONPathQuery_AcceptanceCriteria(t *testing.T) {
	doc := decodeDoc(t, chatDoc)

	a, b := values(t, doc, "$.messages[*].content"), values(t, doc, "$.messages.*.content")
	if len(a) != 5 || !reflect.DeepEqual(a, b) {
		t.Errorf("[*] and .* must agree and select all five: %v vs %v", a, b)
	}

	users := values(t, doc, "$.messages[?@.role=='user'].content")
	if len(users) != 3 || users[0] != "first" || users[2] != "last" {
		t.Errorf("user messages in order: %v", users)
	}

	both := values(t, doc, "$.messages[?@.role=='user' || @.role=='tool'].content")
	if len(both) != 4 {
		t.Errorf("user or tool: %v", both)
	}
	notAssistant := values(t, doc, "$.messages[?!(@.role=='assistant')].content")
	if len(notAssistant) != 4 {
		t.Errorf("not assistant: %v", notAssistant)
	}
	if got := values(t, doc, "$.messages[-1].content"); !reflect.DeepEqual(got, []any{"last"}) {
		t.Errorf("negative index: %v", got)
	}
	if got := values(t, doc, "$.messages[-2:].role"); !reflect.DeepEqual(got, []any{"tool", "user"}) {
		t.Errorf("slice: %v", got)
	}
}

func TestJSONPathQuery_NormalizedPathsLocateTheNodes(t *testing.T) {
	doc := decodeDoc(t, `{"a b":{"it's":[10,20,{"k\n":1}]},"é":2}`)
	nodes, err := QueryJSONPath(doc, `$..*`)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		again, err := QueryJSONPath(doc, n.Path)
		if err != nil || len(again) != 1 || !reflect.DeepEqual(again[0].Value, n.Value) || again[0].Path != n.Path {
			t.Errorf("path %s does not find its own node: %v %v", n.Path, again, err)
		}
	}
	got, _ := QueryJSONPath(doc, `$['a b']['it\'s'][2]['k\n']`)
	if len(got) != 1 || got[0].Path != `$['a b']['it\'s'][2]['k\n']` {
		t.Errorf("%v", got)
	}
}

func TestJSONPathQuery_NothingIsNotNull(t *testing.T) {
	doc := decodeDoc(t, `[{"a":null},{"b":1},{"a":0}]`)
	if got := values(t, doc, `$[?@.a==null]`); len(got) != 1 {
		t.Errorf("a==null: %v", got)
	}
	if got := values(t, doc, `$[?@.a!=null]`); len(got) != 2 {
		t.Errorf("a!=null includes the one without a: %v", got)
	}
	if got := values(t, doc, `$[?@.missing==@.alsoMissing]`); len(got) != 3 {
		t.Errorf("Nothing equals Nothing: %v", got)
	}
	if got := values(t, doc, `$[?@.a]`); len(got) != 2 {
		t.Errorf("existence is about presence, not truth: %v", got)
	}
}

func TestJSONPathQuery_Functions(t *testing.T) {
	doc := decodeDoc(t, `{"items":[{"n":"abc","t":[1,2]},{"n":"héé","t":[]},{"n":"x"}]}`)
	cases := map[string]int{
		`$.items[?length(@.n)==3]`:         2, // runes, not bytes
		`$.items[?count(@.t[*])==2]`:       1,
		`$.items[?match(@.n, 'a.c')]`:      1,
		`$.items[?search(@.n, 'é')]`:       1,
		`$.items[?value(@.t[0])==1]`:       1,
		`$.items[?match(@.n, 'b')]`:        0, // match is anchored
		`$.items[?search(@.n, 'b')]`:       1,
		`$.items[?length(@.t)==0]`:         1,
		`$.items[?length(@.missing)==0]`:   0,
		`$.items[?match(@.n, '[')]`:        0, // an invalid pattern matches nothing
		`$.items[?match(@.n, 5)]`:          0,
		`$.items[?length(@.n)>=2&&@.t]`:    2,
		`$.items[?(@.n=='x')||(@.n=='y')]`: 1,
	}
	for path, want := range cases {
		if got := values(t, doc, path); len(got) != want {
			t.Errorf("%s: got %d want %d (%v)", path, len(got), want, got)
		}
	}
}

func TestJSONPathQuery_InvalidPathsReportWhy(t *testing.T) {
	for _, path := range []string{
		``, `a.b`, `$.`, `$..`, `$[`, `$[?`, `$['a'`, `$[01]`, `$[-0]`, `$[1:2:3:4]`, `$[?@.a==]`, `$[?length(@.a)]`,
		`$[?match(@.a)]`, `$[?count(1)==1]`, `$[?@.*==1]`, `$[?1]`, `$[?foo(@)]`, `$['a\q']`, "$['a\nb']", `$.a b`, `$[9007199254740992]`,
		`$[?@.a==1 &&]`, `$ .a `,
	} {
		_, err := CompileJSONPath(path)
		if err == nil {
			t.Errorf("%q should not compile", path)
		} else if !errors.Is(err, ErrInvalidJSONPath) {
			t.Errorf("%q: error does not wrap ErrInvalidJSONPath: %v", path, err)
		}
	}
	deep := "$[?" + repeat("(", 200) + "@.a" + repeat(")", 200) + "]"
	if _, err := CompileJSONPath(deep); err == nil {
		t.Error("an absurdly nested filter should be refused, not recursed into")
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestJSONPathQuery_NoMatchIsNotAnError(t *testing.T) {
	got, err := QueryJSONPath(decodeDoc(t, `{"a":1}`), `$.b.c[*]`)
	if err != nil || len(got) != 0 {
		t.Errorf("%v %v", got, err)
	}
}

func TestJSONPathQuery_SingularAndSlicesAndDescendants(t *testing.T) {
	for path, want := range map[string]bool{`$.a.b[0]`: true, `$['a']`: true, `$.a[*]`: false, `$..a`: false, `$.a[0,1]`: false, `$.a[1:2]`: false, `$.a[?@.x]`: false} {
		q, err := CompileJSONPath(path)
		if err != nil || q.IsSingular() != want {
			t.Errorf("%s: singular=%v err=%v", path, q != nil && q.IsSingular(), err)
		}
	}
	doc := decodeDoc(t, `{"a":[0,1,2,3,4,5],"o":{"x":{"a":"deep"}}}`)
	for path, want := range map[string][]any{
		`$.a[::2]`:    {0.0, 2.0, 4.0},
		`$.a[::-2]`:   {5.0, 3.0, 1.0},
		`$.a[4:1:-1]`: {4.0, 3.0, 2.0},
		`$.a[10:]`:    nil,
		`$.a[::0]`:    nil,
		`$..a`:        {[]any{0.0, 1.0, 2.0, 3.0, 4.0, 5.0}, "deep"},
		`$.a[1,-1]`:   {1.0, 5.0},
	} {
		if got := values(t, doc, path); !(len(got) == 0 && len(want) == 0) && !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", path, got, want)
		}
	}
}

func TestJSONPathQuery_NumbersFromAnyDecoder(t *testing.T) {
	d := json.NewDecoder(stringsReader(`[{"n":4155550199},{"n":1.5}]`))
	d.UseNumber()
	var doc any
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if got := values(t, doc, `$[?@.n==4155550199]`); len(got) != 1 {
		t.Errorf("json.Number: %v", got)
	}
	if got := values(t, map[string]any{"a": []any{1, int64(2), 3.0}}, `$.a[?@>1]`); len(got) != 2 {
		t.Errorf("Go integer types: %v", got)
	}
}

func TestJSONPathQuery_DeterministicAndConcurrent(t *testing.T) {
	doc := decodeDoc(t, `{"z":1,"a":2,"m":{"b":3,"a":4}}`)
	q, err := CompileJSONPath(`$..*`)
	if err != nil {
		t.Fatal(err)
	}
	first := q.Select(doc)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !reflect.DeepEqual(q.Select(doc), first) {
				t.Error("selection order changed")
			}
		}()
	}
	wg.Wait()
}

// The helpers that existed before are untouched: same syntax, same results.
func TestExistingHelpersKeepTheirBehaviour(t *testing.T) {
	doc := decodeDoc(t, chatDoc).(map[string]any)
	if v, err := ExtractValueFromJsonpath(doc, "$.messages[-1].content"); err != nil || v != "last" {
		t.Errorf("%v %v", v, err)
	}
	if v, err := ExtractValueFromJsonpath(doc, "messages[0].role"); err != nil || v != "user" {
		t.Errorf("a path without $ still works: %v %v", v, err)
	}
	if v, err := ExtractValueFromJsonpath(doc, "$.messages.*.role"); err != nil || len(v.([]interface{})) != 5 {
		t.Errorf("%v %v", v, err)
	}
	if _, err := ExtractValueFromJsonpath(doc, "$.messages[*].content"); err == nil || err.Error() != "key not found: messages[*]" {
		t.Errorf("the old helper must keep rejecting [*] exactly as before: %v", err)
	}
	if err := SetValueAtJSONPath(doc, "$.model", "x"); err != nil || doc["model"] != "x" {
		t.Errorf("%v", err)
	}
}

func FuzzJSONPathQuery(f *testing.F) {
	for _, s := range []string{`$`, `$.a`, `$..*`, `$[?@.a==1]`, `$[1:2:3]`, `$['a','b']`, `$[?match(@.a,'x.*')]`, `$[?length(@)>1&&!(@.b)]`, `$.a[?count(@..b)==2]`} {
		f.Add(s)
	}
	doc := decodeDocFuzz()
	f.Fuzz(func(t *testing.T, path string) {
		q, err := CompileJSONPath(path)
		if err != nil {
			if !errors.Is(err, ErrInvalidJSONPath) {
				t.Fatalf("error without ErrInvalidJSONPath: %v", err)
			}
			return
		}
		for _, n := range q.Select(doc) {
			again, err := QueryJSONPath(doc, n.Path)
			if err != nil || len(again) != 1 {
				t.Fatalf("normalized path %q of %q does not reselect its node: %v %v", n.Path, path, again, err)
			}
		}
	})
}

func decodeDocFuzz() any {
	var v any
	_ = json.Unmarshal([]byte(`{"a":[1,"x",{"b":[true,null]},[]],"b":{"a":"xy","c":2.5},"d":"é"}`), &v)
	return v
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }
