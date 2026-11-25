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
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// This file implements JSONPath as specified by RFC 9535 (JSONPath: Query
// Expressions for JSON): child and descendant segments, name, index, wildcard,
// slice and filter selectors, and the length, count, match, search and value
// functions.
//
// It sits next to the older helpers in jsonpath.go and does not replace them:
// ExtractValueFromJsonpath, ExtractStringValueFromJsonpath and SetValueAtJSONPath
// keep their syntax and results. The functions here are for callers that need the
// full syntax, for example "$.messages[?@.role=='user'].content" or
// "$.messages[*].content", and a list of every match.
//
// A query runs over the values encoding/json produces: map[string]any, []any,
// string, bool, nil and numbers (float64, json.Number or any Go integer type). It
// has no dependencies, in keeping with the rest of the SDK.

// ErrInvalidJSONPath is wrapped by every error CompileJSONPath returns, so a
// caller can tell a path that does not parse from a path that matches nothing.
var ErrInvalidJSONPath = errors.New("invalid JSONPath")

// JSONPathNode is one value a query selected, with its location.
type JSONPathNode struct {
	// Path is the normalized path of the node (RFC 9535 section 2.7), for example
	// $['messages'][0]['content']. It is a valid query that selects exactly this
	// node, so it can be used to find the node again in another representation
	// of the same document.
	Path string
	// Value is the selected value.
	Value any
}

// JSONPathQuery is a compiled RFC 9535 query. It is immutable and safe for
// concurrent use.
type JSONPathQuery struct {
	src      string
	segments []jpSegment
	singular bool
}

// CompileJSONPath parses an RFC 9535 query. The error wraps ErrInvalidJSONPath and
// says where the problem is.
func CompileJSONPath(path string) (*JSONPathQuery, error) {
	p := &jpParser{s: path}
	segs, err := p.parseQuery()
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, p.errorf("unexpected %q", p.rest())
	}
	return &JSONPathQuery{src: path, segments: segs, singular: segmentsSingular(segs)}, nil
}

// String returns the query text.
func (q *JSONPathQuery) String() string { return q.src }

// IsSingular reports whether the query can select at most one node: it uses only
// child segments with a single name or index selector.
func (q *JSONPathQuery) IsSingular() bool { return q.singular }

// Select returns the nodes the query selects in root, in the order the RFC
// defines: segment by segment, selector by selector, object members by name. It
// returns an empty list, not an error, when nothing matches.
func (q *JSONPathQuery) Select(root any) []JSONPathNode {
	c := &jpEval{root: root, paths: true}
	nodes := evalSegments(c, q.segments, []jpNode{{val: root}})
	out := make([]JSONPathNode, len(nodes))
	for i, n := range nodes {
		out[i] = JSONPathNode{Path: n.path.String(), Value: n.val}
	}
	return out
}

// QueryJSONPath compiles path and selects its nodes in root.
func QueryJSONPath(root any, path string) ([]JSONPathNode, error) {
	q, err := CompileJSONPath(path)
	if err != nil {
		return nil, err
	}
	return q.Select(root), nil
}

// QueryJSONPathValues is QueryJSONPath without the locations.
func QueryJSONPathValues(root any, path string) ([]any, error) {
	nodes, err := QueryJSONPath(root, path)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(nodes))
	for i, n := range nodes {
		out[i] = n.Value
	}
	return out, nil
}

// ─── data model ──────────────────────────────────────────────────────────────

// jpPath is the location of a node as a chain of steps. The normalized path
// string is built only for nodes a query returns, so a query that visits many
// nodes does not pay for a path per visit.
type jpPath struct {
	parent *jpPath
	name   string
	idx    int
	isName bool
}

type jpNode struct {
	path *jpPath // nil for the root, and when paths are not wanted
	val  any
}

type jpEval struct {
	root  any
	paths bool
}

func (c *jpEval) nameChild(parent jpNode, name string) *jpPath {
	if !c.paths {
		return nil
	}
	return &jpPath{parent: parent.path, name: name, isName: true}
}

func (c *jpEval) idxChild(parent jpNode, idx int) *jpPath {
	if !c.paths {
		return nil
	}
	return &jpPath{parent: parent.path, idx: idx}
}

// String renders the normalized path (RFC 9535 section 2.7).
func (p *jpPath) String() string {
	var steps []*jpPath
	for cur := p; cur != nil; cur = cur.parent {
		steps = append(steps, cur)
	}
	var b strings.Builder
	b.WriteByte('$')
	for i := len(steps) - 1; i >= 0; i-- {
		st := steps[i]
		if st.isName {
			b.WriteString("['")
			b.WriteString(escapeNormalizedName(st.name))
			b.WriteString("']")
		} else {
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(st.idx))
			b.WriteByte(']')
		}
	}
	return b.String()
}

// ─── AST ─────────────────────────────────────────────────────────────────────

type jpSegment struct {
	descendant bool
	selectors  []jpSelector
}

type jpSelector interface {
	// apply appends the children of n this selector picks.
	apply(c *jpEval, n jpNode, out []jpNode) []jpNode
	singularSel() bool
}

type jpName struct{ name string }
type jpIndex struct{ idx int64 }
type jpWildcard struct{}
type jpSlice struct {
	start, end, step int64
	hasStart, hasEnd bool
}
type jpFilter struct{ cond jpLogical }

func (jpName) singularSel() bool     { return true }
func (jpIndex) singularSel() bool    { return true }
func (jpWildcard) singularSel() bool { return false }
func (jpSlice) singularSel() bool    { return false }
func (jpFilter) singularSel() bool   { return false }

func segmentsSingular(segs []jpSegment) bool {
	for _, s := range segs {
		if s.descendant || len(s.selectors) != 1 || !s.selectors[0].singularSel() {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s jpName) apply(c *jpEval, n jpNode, out []jpNode) []jpNode {
	if m, ok := n.val.(map[string]any); ok {
		if v, ok := m[s.name]; ok {
			out = append(out, jpNode{path: c.nameChild(n, s.name), val: v})
		}
	}
	return out
}

func (s jpIndex) apply(c *jpEval, n jpNode, out []jpNode) []jpNode {
	if a, ok := n.val.([]any); ok {
		i := s.idx
		if i < 0 {
			i += int64(len(a))
		}
		if i >= 0 && i < int64(len(a)) {
			out = append(out, jpNode{path: c.idxChild(n, int(i)), val: a[i]})
		}
	}
	return out
}

func (jpWildcard) apply(c *jpEval, n jpNode, out []jpNode) []jpNode {
	switch v := n.val.(type) {
	case []any:
		for i, e := range v {
			out = append(out, jpNode{path: c.idxChild(n, i), val: e})
		}
	case map[string]any:
		for _, k := range sortedKeys(v) {
			out = append(out, jpNode{path: c.nameChild(n, k), val: v[k]})
		}
	}
	return out
}

func (s jpSlice) apply(c *jpEval, n jpNode, out []jpNode) []jpNode {
	a, ok := n.val.([]any)
	if !ok || s.step == 0 {
		return out
	}
	length := int64(len(a))
	norm := func(i int64) int64 {
		if i >= 0 {
			return i
		}
		return length + i
	}
	pick := func(i int64) jpNode {
		return jpNode{path: c.idxChild(n, int(i)), val: a[i]}
	}
	// RFC 9535 section 2.3.4.2.2: defaults, normalisation, bounds.
	if s.step > 0 {
		start, end := int64(0), length
		if s.hasStart {
			start = s.start
		}
		if s.hasEnd {
			end = s.end
		}
		lower := min64(max64(norm(start), 0), length)
		upper := min64(max64(norm(end), 0), length)
		for i := lower; i < upper; i += s.step {
			out = append(out, pick(i))
		}
		return out
	}
	start, end := length-1, -length-1
	if s.hasStart {
		start = s.start
	}
	if s.hasEnd {
		end = s.end
	}
	upper := min64(max64(norm(start), -1), length-1)
	lower := min64(max64(norm(end), -1), length-1)
	for i := upper; lower < i; i += s.step {
		out = append(out, pick(i))
	}
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s jpFilter) apply(c *jpEval, n jpNode, out []jpNode) []jpNode {
	switch v := n.val.(type) {
	case []any:
		for i, e := range v {
			if s.cond.eval(c, e) {
				out = append(out, jpNode{path: c.idxChild(n, i), val: e})
			}
		}
	case map[string]any:
		for _, k := range sortedKeys(v) {
			if s.cond.eval(c, v[k]) {
				out = append(out, jpNode{path: c.nameChild(n, k), val: v[k]})
			}
		}
	}
	return out
}

func evalSegments(c *jpEval, segs []jpSegment, in []jpNode) []jpNode {
	cur := in
	for _, seg := range segs {
		var next []jpNode
		for _, n := range cur {
			if seg.descendant {
				next = descend(c, seg.selectors, n, next)
			} else {
				next = applySelectors(c, seg.selectors, n, next)
			}
		}
		cur = next
	}
	return cur
}

func applySelectors(c *jpEval, sels []jpSelector, n jpNode, out []jpNode) []jpNode {
	for _, s := range sels {
		out = s.apply(c, n, out)
	}
	return out
}

// descend applies the selectors to n and then to every descendant of n, parents
// before children, in document order.
func descend(c *jpEval, sels []jpSelector, n jpNode, out []jpNode) []jpNode {
	out = applySelectors(c, sels, n, out)
	switch v := n.val.(type) {
	case []any:
		for i, e := range v {
			out = descend(c, sels, jpNode{path: c.idxChild(n, i), val: e}, out)
		}
	case map[string]any:
		for _, k := range sortedKeys(v) {
			out = descend(c, sels, jpNode{path: c.nameChild(n, k), val: v[k]}, out)
		}
	}
	return out
}

// escapeNormalizedName escapes a member name for a normalized path (RFC 9535
// section 2.7): the quote and the backslash, and control characters.
func escapeNormalizedName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// ─── filter expressions ──────────────────────────────────────────────────────

// jpVal is a value of the filter expression type system: a JSON value, or
// Nothing (present == false), which is not the same as null.
type jpVal struct {
	present bool
	v       any
}

var jpNothing = jpVal{}

type jpLogical interface {
	eval(c *jpEval, cur any) bool
}

type jpOr struct{ l, r jpLogical }
type jpAnd struct{ l, r jpLogical }
type jpNot struct{ e jpLogical }
type jpExists struct{ q *jpSubQuery }
type jpCompare struct {
	l, r jpComparable
	op   string
}
type jpFuncTest struct{ f jpFunc }

func (e jpOr) eval(c *jpEval, cur any) bool  { return e.l.eval(c, cur) || e.r.eval(c, cur) }
func (e jpAnd) eval(c *jpEval, cur any) bool { return e.l.eval(c, cur) && e.r.eval(c, cur) }
func (e jpNot) eval(c *jpEval, cur any) bool { return !e.e.eval(c, cur) }
func (e jpExists) eval(c *jpEval, cur any) bool {
	return len(e.q.run(c, cur)) > 0
}
func (e jpFuncTest) eval(c *jpEval, cur any) bool { return e.f.logical(c, cur) }

func (e jpCompare) eval(c *jpEval, cur any) bool {
	l, r := e.l.value(c, cur), e.r.value(c, cur)
	switch e.op {
	case "==":
		return jpEqual(l, r)
	case "!=":
		return !jpEqual(l, r)
	case "<":
		return jpLess(l, r)
	case ">":
		return jpLess(r, l)
	case "<=":
		return jpLess(l, r) || jpEqual(l, r)
	case ">=":
		return jpLess(r, l) || jpEqual(l, r)
	}
	return false
}

type jpComparable interface {
	value(c *jpEval, cur any) jpVal
}

type jpLiteral struct{ v any }
type jpSingular struct{ q *jpSubQuery }

func (l jpLiteral) value(*jpEval, any) jpVal { return jpVal{present: true, v: l.v} }
func (s jpSingular) value(c *jpEval, cur any) jpVal {
	nodes := s.q.run(c, cur)
	if len(nodes) == 1 {
		return jpVal{present: true, v: nodes[0].val}
	}
	return jpNothing
}

// jpSubQuery is a query inside a filter: relative to the current node (@) or
// absolute (the root of the document, $).
type jpSubQuery struct {
	abs      bool
	segments []jpSegment
	singular bool
}

func (q *jpSubQuery) run(c *jpEval, cur any) []jpNode {
	// Locations inside a filter are never reported, so they are not built.
	sub := &jpEval{root: c.root, paths: false}
	start := cur
	if q.abs {
		start = c.root
	}
	return evalSegments(sub, q.segments, []jpNode{{val: start}})
}

func jpNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// jpNumCmp compares two numbers. Numbers that float64 holds exactly are compared
// as floats; an integer or a json.Number beyond 2^53 is compared exactly, so that
// two different 64-bit ids do not read as equal. ok is false when either value is
// not a number or is NaN.
func jpNumCmp(a, b any) (int, bool) {
	fa, oka := jpNumber(a)
	fb, okb := jpNumber(b)
	if !oka || !okb || math.IsNaN(fa) || math.IsNaN(fb) {
		return 0, false
	}
	const exact = 1 << 53
	if math.Abs(fa) < exact && math.Abs(fb) < exact {
		switch {
		case fa < fb:
			return -1, true
		case fa > fb:
			return 1, true
		}
		return 0, true
	}
	ra, rb := jpRat(a), jpRat(b)
	if ra == nil || rb == nil {
		switch {
		case fa < fb:
			return -1, true
		case fa > fb:
			return 1, true
		}
		return 0, true
	}
	return ra.Cmp(rb), true
}

// jpRat is the exact value of a number, or nil when it has none (infinity).
func jpRat(v any) *big.Rat {
	switch n := v.(type) {
	case json.Number:
		r, ok := new(big.Rat).SetString(n.String())
		if !ok {
			return nil
		}
		return r
	case int:
		return new(big.Rat).SetInt64(int64(n))
	case int8:
		return new(big.Rat).SetInt64(int64(n))
	case int16:
		return new(big.Rat).SetInt64(int64(n))
	case int32:
		return new(big.Rat).SetInt64(int64(n))
	case int64:
		return new(big.Rat).SetInt64(n)
	case uint:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(uint64(n)))
	case uint8:
		return new(big.Rat).SetInt64(int64(n))
	case uint16:
		return new(big.Rat).SetInt64(int64(n))
	case uint32:
		return new(big.Rat).SetInt64(int64(n))
	case uint64:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(n))
	}
	f, _ := jpNumber(v)
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return nil
	}
	return new(big.Rat).SetFloat64(f)
}

func jpEqual(a, b jpVal) bool {
	if !a.present || !b.present {
		return !a.present && !b.present
	}
	return jpDeepEqual(a.v, b.v)
}

func jpDeepEqual(a, b any) bool {
	if _, ok := jpNumber(a); ok {
		c, ok := jpNumCmp(a, b)
		return ok && c == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jpDeepEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !jpDeepEqual(xv, yv) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// jpLess is the "<" of RFC 9535: defined for two numbers or two strings only.
func jpLess(a, b jpVal) bool {
	if !a.present || !b.present {
		return false
	}
	if _, ok := jpNumber(a.v); ok {
		c, ok := jpNumCmp(a.v, b.v)
		return ok && c < 0
	}
	if sa, ok := a.v.(string); ok {
		sb, ok := b.v.(string)
		return ok && compareByCodePoint(sa, sb) < 0
	}
	return false
}

// compareByCodePoint orders strings by Unicode scalar value (UTF-8 byte order
// gives the same order, but lone surrogates and invalid bytes are compared as
// the replacement character would be, so decode explicitly).
func compareByCodePoint(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		ra, wa := utf8.DecodeRuneInString(a)
		rb, wb := utf8.DecodeRuneInString(b)
		if ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		a, b = a[wa:], b[wb:]
	}
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return -1
	}
	return 1
}

// ─── functions ───────────────────────────────────────────────────────────────

type jpFunc struct {
	name string
	args []jpArg
}

// jpArg is one argument: a literal, a query, or a function call.
type jpArg struct {
	lit   *jpLiteral
	query *jpSubQuery
	fn    *jpFunc
}

// valueArg evaluates an argument that a ValueType parameter takes.
func (a jpArg) valueArg(c *jpEval, cur any) jpVal {
	switch {
	case a.lit != nil:
		return jpVal{present: true, v: a.lit.v}
	case a.query != nil:
		nodes := a.query.run(c, cur)
		if len(nodes) == 1 {
			return jpVal{present: true, v: nodes[0].val}
		}
		return jpNothing
	case a.fn != nil:
		return a.fn.value(c, cur)
	}
	return jpNothing
}

// value evaluates a function whose result is a ValueType.
func (f jpFunc) value(c *jpEval, cur any) jpVal {
	switch f.name {
	case "length":
		v := f.args[0].valueArg(c, cur)
		if !v.present {
			return jpNothing
		}
		switch x := v.v.(type) {
		case string:
			return jpVal{present: true, v: float64(utf8.RuneCountInString(x))}
		case []any:
			return jpVal{present: true, v: float64(len(x))}
		case map[string]any:
			return jpVal{present: true, v: float64(len(x))}
		}
		return jpNothing
	case "count":
		return jpVal{present: true, v: float64(len(f.args[0].query.run(c, cur)))}
	case "value":
		nodes := f.args[0].query.run(c, cur)
		if len(nodes) == 1 {
			return jpVal{present: true, v: nodes[0].val}
		}
		return jpNothing
	}
	return jpNothing
}

// logical evaluates a function whose result is a LogicalType.
func (f jpFunc) logical(c *jpEval, cur any) bool {
	a, b := f.args[0].valueArg(c, cur), f.args[1].valueArg(c, cur)
	s, ok1 := a.v.(string)
	p, ok2 := b.v.(string)
	if !a.present || !b.present || !ok1 || !ok2 {
		return false
	}
	re := compileIRegexp(p, f.name == "match")
	return re != nil && re.MatchString(s)
}

type jpFuncValue struct{ f jpFunc }

func (v jpFuncValue) value(c *jpEval, cur any) jpVal { return v.f.value(c, cur) }

// iregexpCacheMax bounds the compiled patterns kept. A pattern normally comes
// from the query, so there are few; one that comes from the document (search(@.a,
// @.b)) is chosen by whoever wrote the document, so the cache must not grow with it.
const iregexpCacheMax = 512

var (
	iregexpMu    sync.Mutex
	iregexpCache = map[string]*regexp.Regexp{} // nil value: the pattern is invalid
)

// compileIRegexp compiles an I-Regexp (RFC 9485) pattern: anchored for match,
// unanchored for search. "." means any character except line feed and carriage
// return. A pattern that is not a valid I-Regexp gives nil, which the caller treats
// as "no match", as RFC 9535 says.
func compileIRegexp(pattern string, anchored bool) *regexp.Regexp {
	key := "s|" + pattern
	if anchored {
		key = "m|" + pattern
	}
	iregexpMu.Lock()
	re, ok := iregexpCache[key]
	iregexpMu.Unlock()
	if ok {
		return re
	}
	re = nil
	if validIRegexp(pattern) {
		expr := rewriteDot(pattern)
		// The pattern must compile on its own, so that something like "a)|(b"
		// cannot close the group that anchors it and escape the anchors.
		if _, err := regexp.Compile(expr); err == nil {
			if anchored {
				expr = `\A(?:` + expr + `)\z`
			}
			if c, err := regexp.Compile(expr); err == nil {
				re = c
			}
		}
	}
	iregexpMu.Lock()
	if len(iregexpCache) >= iregexpCacheMax {
		iregexpCache = map[string]*regexp.Regexp{}
	}
	iregexpCache[key] = re
	iregexpMu.Unlock()
	return re
}

// validIRegexp reports whether p only uses what RFC 9485 allows, where Go's
// regexp accepts more: no (?...) groups, no lazy quantifiers, no escapes other than
// the single characters I-Regexp names and \p{..} \P{..}, and "{" only as a
// quantifier. "^" and "$" are accepted as anchors, as the RFC 9535 compliance
// suite expects.
func validIRegexp(p string) bool {
	inClass := false
	prevQuant := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		isQuant := false
		switch {
		case c == '\\':
			i++
			if i >= len(p) {
				return false
			}
			switch p[i] {
			case '\\', '|', '.', '-', '^', '?', '*', '+', '{', '}', '(', ')', '[', ']', 'n', 'r', 't', '$':
			case 'p', 'P':
				if i+1 >= len(p) || p[i+1] != '{' {
					return false
				}
				end := strings.IndexByte(p[i+1:], '}')
				if end < 2 {
					return false
				}
				i += 1 + end
			default:
				return false
			}
		case inClass:
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
			if i+1 < len(p) && p[i+1] == '^' {
				i++
			}
			if i+1 < len(p) && p[i+1] == ']' {
				return false // an empty class
			}
		case c == '(':
			if i+1 < len(p) && p[i+1] == '?' {
				return false
			}
		case c == '*' || c == '+' || c == '?':
			if prevQuant && c == '?' {
				return false // a lazy quantifier
			}
			isQuant = true
		case c == '{':
			j := i + 1
			for j < len(p) && p[j] >= '0' && p[j] <= '9' {
				j++
			}
			if j == i+1 {
				return false
			}
			if j < len(p) && p[j] == ',' {
				j++
				for j < len(p) && p[j] >= '0' && p[j] <= '9' {
					j++
				}
			}
			if j >= len(p) || p[j] != '}' {
				return false
			}
			i = j
			isQuant = true
		case c == '}':
			return false
		}
		prevQuant = isQuant
	}
	return !inClass
}

// rewriteDot turns an unescaped "." outside a character class into [^\n\r].
func rewriteDot(p string) string {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		ch := p[i]
		switch {
		case ch == '\\' && i+1 < len(p):
			b.WriteByte(ch)
			i++
			b.WriteByte(p[i])
		case ch == '[' && !inClass:
			inClass = true
			b.WriteByte(ch)
		case ch == ']' && inClass:
			inClass = false
			b.WriteByte(ch)
		case ch == '.' && !inClass:
			b.WriteString(`[^\n\r]`)
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// ─── parser ──────────────────────────────────────────────────────────────────

const jpMaxInt = int64(1)<<53 - 1

type jpParser struct {
	s     string
	i     int
	depth int
}

const jpMaxDepth = 100

func (p *jpParser) errorf(format string, a ...any) error {
	return fmt.Errorf("%w at offset %d: %s", ErrInvalidJSONPath, p.i, fmt.Sprintf(format, a...))
}

func (p *jpParser) rest() string {
	r := p.s[p.i:]
	if len(r) > 12 {
		return r[:12] + "..."
	}
	return r
}

func (p *jpParser) eof() bool { return p.i >= len(p.s) }

func (p *jpParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}

func (p *jpParser) skipSpace() {
	for !p.eof() {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jpParser) consume(tok string) bool {
	if strings.HasPrefix(p.s[p.i:], tok) {
		p.i += len(tok)
		return true
	}
	return false
}

func (p *jpParser) parseQuery() ([]jpSegment, error) {
	if !p.consume("$") {
		return nil, p.errorf("a query starts with $")
	}
	return p.parseSegments()
}

// parseSegments reads zero or more segments, each optionally preceded by blank
// space.
func (p *jpParser) parseSegments() ([]jpSegment, error) {
	var segs []jpSegment
	for {
		save := p.i
		p.skipSpace()
		if p.eof() {
			p.i = save
			// trailing blank space is not part of a query
			return segs, nil
		}
		switch {
		case strings.HasPrefix(p.s[p.i:], ".."):
			p.i += 2
			sels, err := p.parseSegmentBody(true)
			if err != nil {
				return nil, err
			}
			segs = append(segs, jpSegment{descendant: true, selectors: sels})
		case p.peek() == '.':
			p.i++
			sels, err := p.parseSegmentBody(false)
			if err != nil {
				return nil, err
			}
			segs = append(segs, jpSegment{selectors: sels})
		case p.peek() == '[':
			sels, err := p.parseBracketed()
			if err != nil {
				return nil, err
			}
			segs = append(segs, jpSegment{selectors: sels})
		default:
			p.i = save
			return segs, nil
		}
	}
}

// parseSegmentBody reads what follows "." or "..": a wildcard, a member name, or
// (after "..") a bracketed selection.
func (p *jpParser) parseSegmentBody(descendant bool) ([]jpSelector, error) {
	switch {
	case p.peek() == '*':
		p.i++
		return []jpSelector{jpWildcard{}}, nil
	case p.peek() == '[' && descendant:
		return p.parseBracketed()
	default:
		name, ok := p.parseShorthandName()
		if !ok {
			return nil, p.errorf("expected a member name or * after .")
		}
		return []jpSelector{jpName{name: name}}, nil
	}
}

func isNameFirst(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r >= 0x80
}

func isNameChar(r rune) bool { return isNameFirst(r) || (r >= '0' && r <= '9') }

func (p *jpParser) parseShorthandName() (string, bool) {
	start := p.i
	r, w := utf8.DecodeRuneInString(p.s[p.i:])
	if w == 0 || !isNameFirst(r) {
		return "", false
	}
	p.i += w
	for !p.eof() {
		r, w = utf8.DecodeRuneInString(p.s[p.i:])
		if !isNameChar(r) {
			break
		}
		p.i += w
	}
	return p.s[start:p.i], true
}

func (p *jpParser) parseBracketed() ([]jpSelector, error) {
	p.i++ // [
	var sels []jpSelector
	for {
		p.skipSpace()
		sel, err := p.parseSelector()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
		p.skipSpace()
		if p.consume(",") {
			continue
		}
		if p.consume("]") {
			return sels, nil
		}
		return nil, p.errorf("expected , or ]")
	}
}

func (p *jpParser) parseSelector() (jpSelector, error) {
	switch c := p.peek(); {
	case c == '*':
		p.i++
		return jpWildcard{}, nil
	case c == '\'' || c == '"':
		s, err := p.parseStringLiteral()
		if err != nil {
			return nil, err
		}
		return jpName{name: s}, nil
	case c == '?':
		p.i++
		p.skipSpace()
		cond, err := p.parseLogicalOr()
		if err != nil {
			return nil, err
		}
		return jpFilter{cond: cond}, nil
	default:
		return p.parseIndexOrSlice()
	}
}

// parseInt reads an RFC 9535 int: "0", or a non-zero digit run, optionally
// negative, with no leading zeros and no "-0", within the I-JSON range.
func (p *jpParser) parseInt() (int64, bool, error) {
	start := p.i
	if p.peek() == '-' {
		p.i++
	}
	ds := p.i
	for !p.eof() && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	digits := p.s[ds:p.i]
	if digits == "" {
		p.i = start
		return 0, false, nil
	}
	if len(digits) > 1 && digits[0] == '0' {
		return 0, false, p.errorf("leading zero in %q", p.s[start:p.i])
	}
	if p.s[start] == '-' && digits == "0" {
		return 0, false, p.errorf("-0 is not an index")
	}
	n, err := strconv.ParseInt(p.s[start:p.i], 10, 64)
	if err != nil || n > jpMaxInt || n < -jpMaxInt {
		return 0, false, p.errorf("integer %q out of range", p.s[start:p.i])
	}
	return n, true, nil
}

func (p *jpParser) parseIndexOrSlice() (jpSelector, error) {
	var sl jpSlice
	first, hasFirst, err := p.parseInt()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.peek() != ':' {
		if !hasFirst {
			return nil, p.errorf("expected a selector")
		}
		return jpIndex{idx: first}, nil
	}
	p.i++ // :
	sl.start, sl.hasStart = first, hasFirst
	p.skipSpace()
	end, hasEnd, err := p.parseInt()
	if err != nil {
		return nil, err
	}
	sl.end, sl.hasEnd = end, hasEnd
	p.skipSpace()
	sl.step = 1
	if p.peek() == ':' {
		p.i++
		p.skipSpace()
		step, hasStep, err := p.parseInt()
		if err != nil {
			return nil, err
		}
		if hasStep {
			sl.step = step
		}
	}
	return sl, nil
}

// parseStringLiteral reads a single- or double-quoted string with the RFC 9535
// escapes. Unescaped control characters are not allowed.
func (p *jpParser) parseStringLiteral() (string, error) {
	q := p.s[p.i]
	p.i++
	var b strings.Builder
	for {
		if p.eof() {
			return "", p.errorf("unterminated string")
		}
		r, w := utf8.DecodeRuneInString(p.s[p.i:])
		switch {
		case r == rune(q):
			p.i++
			return b.String(), nil
		case r == '\\':
			p.i++
			if p.eof() {
				return "", p.errorf("unterminated escape")
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '/', '\\':
				b.WriteByte(e)
			case '\'', '"':
				if e != q {
					return "", p.errorf("escape \\%c is not allowed in a %c string", e, q)
				}
				b.WriteByte(e)
			case 'u':
				r1, err := p.parseHex4()
				if err != nil {
					return "", err
				}
				if r1 >= 0xD800 && r1 <= 0xDBFF {
					if !p.consume(`\u`) {
						return "", p.errorf("high surrogate without a low surrogate")
					}
					r2, err := p.parseHex4()
					if err != nil {
						return "", err
					}
					if r2 < 0xDC00 || r2 > 0xDFFF {
						return "", p.errorf("high surrogate followed by a non-low surrogate")
					}
					b.WriteRune(0x10000 + (r1-0xD800)<<10 + (r2 - 0xDC00))
				} else if r1 >= 0xDC00 && r1 <= 0xDFFF {
					return "", p.errorf("lone low surrogate")
				} else {
					b.WriteRune(r1)
				}
			default:
				return "", p.errorf("unknown escape \\%c", e)
			}
		case r < 0x20:
			return "", p.errorf("control character in string")
		case r == utf8.RuneError && w == 1:
			return "", p.errorf("invalid UTF-8 in string")
		default:
			b.WriteRune(r)
			p.i += w
		}
	}
}

func (p *jpParser) parseHex4() (rune, error) {
	if p.i+4 > len(p.s) {
		return 0, p.errorf("short \\u escape")
	}
	var r rune
	for k := 0; k < 4; k++ {
		c := p.s[p.i+k]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, p.errorf("bad hex digit in \\u escape")
		}
		r = r<<4 | rune(d)
	}
	p.i += 4
	return r, nil
}

// ─── filter expression parser ────────────────────────────────────────────────

func (p *jpParser) enter() error {
	p.depth++
	if p.depth > jpMaxDepth {
		return p.errorf("filter nested more than %d levels", jpMaxDepth)
	}
	return nil
}

func (p *jpParser) leave() { p.depth-- }

func (p *jpParser) parseLogicalOr() (jpLogical, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	left, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}
	for {
		save := p.i
		p.skipSpace()
		if !p.consume("||") {
			p.i = save
			return left, nil
		}
		p.skipSpace()
		right, err := p.parseLogicalAnd()
		if err != nil {
			return nil, err
		}
		left = jpOr{left, right}
	}
}

func (p *jpParser) parseLogicalAnd() (jpLogical, error) {
	left, err := p.parseBasic()
	if err != nil {
		return nil, err
	}
	for {
		save := p.i
		p.skipSpace()
		if !p.consume("&&") {
			p.i = save
			return left, nil
		}
		p.skipSpace()
		right, err := p.parseBasic()
		if err != nil {
			return nil, err
		}
		left = jpAnd{left, right}
	}
}

// parseBasic reads a parenthesised expression, a comparison, or an existence or
// function test, each with an optional "!".
func (p *jpParser) parseBasic() (jpLogical, error) {
	not := false
	if p.peek() == '!' && !strings.HasPrefix(p.s[p.i:], "!=") {
		p.i++
		p.skipSpace()
		not = true
	}
	wrap := func(e jpLogical) jpLogical {
		if not {
			return jpNot{e}
		}
		return e
	}
	if p.peek() == '(' {
		p.i++
		p.skipSpace()
		e, err := p.parseLogicalOr()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if !p.consume(")") {
			return nil, p.errorf("expected )")
		}
		return wrap(e), nil
	}
	// A comparison or a test: read the first operand and see what follows.
	start := p.i
	op, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	save := p.i
	p.skipSpace()
	if cmp := p.parseComparisonOp(); cmp != "" {
		if not {
			return nil, p.errorf("! cannot negate a comparison without parentheses")
		}
		p.skipSpace()
		rop, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		l, err := op.asComparable(p, start)
		if err != nil {
			return nil, err
		}
		r, err := rop.asComparable(p, start)
		if err != nil {
			return nil, err
		}
		return jpCompare{l: l, r: r, op: cmp}, nil
	}
	p.i = save
	t, err := op.asTest(p, start)
	if err != nil {
		return nil, err
	}
	return wrap(t), nil
}

func (p *jpParser) parseComparisonOp() string {
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if p.consume(op) {
			return op
		}
	}
	return ""
}

// jpOperand is whatever stands on one side of a comparison, or alone as a test.
type jpOperand struct {
	lit   *jpLiteral
	query *jpSubQuery
	fn    *jpFunc
}

func (o jpOperand) asComparable(p *jpParser, at int) (jpComparable, error) {
	switch {
	case o.lit != nil:
		return *o.lit, nil
	case o.query != nil:
		if !o.query.singular {
			p.i = at
			return nil, p.errorf("a comparison needs a singular query")
		}
		return jpSingular{q: o.query}, nil
	case o.fn != nil:
		if jpFuncResultIsLogical(o.fn.name) {
			p.i = at
			return nil, p.errorf("%s() returns a logical value and cannot be compared", o.fn.name)
		}
		return jpFuncValue{f: *o.fn}, nil
	}
	return nil, p.errorf("expected an operand")
}

func (o jpOperand) asTest(p *jpParser, at int) (jpLogical, error) {
	switch {
	case o.query != nil:
		return jpExists{q: o.query}, nil
	case o.fn != nil:
		if !jpFuncResultIsLogical(o.fn.name) {
			p.i = at
			return nil, p.errorf("%s() returns a value; compare it", o.fn.name)
		}
		return jpFuncTest{f: *o.fn}, nil
	}
	p.i = at
	return nil, p.errorf("a literal is not a test; compare it")
}

func jpFuncResultIsLogical(name string) bool { return name == "match" || name == "search" }

// parseOperand reads a literal, a filter query (@... or $...) or a function call.
func (p *jpParser) parseOperand() (jpOperand, error) {
	switch c := p.peek(); {
	case c == '@' || c == '$':
		q, err := p.parseSubQuery()
		if err != nil {
			return jpOperand{}, err
		}
		return jpOperand{query: q}, nil
	case c == '\'' || c == '"':
		s, err := p.parseStringLiteral()
		if err != nil {
			return jpOperand{}, err
		}
		return jpOperand{lit: &jpLiteral{v: s}}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		n, err := p.parseNumberLiteral()
		if err != nil {
			return jpOperand{}, err
		}
		return jpOperand{lit: &jpLiteral{v: n}}, nil
	case c >= 'a' && c <= 'z':
		return p.parseWordOperand()
	}
	return jpOperand{}, p.errorf("expected a literal, a query or a function")
}

func (p *jpParser) parseSubQuery() (*jpSubQuery, error) {
	abs := p.s[p.i] == '$'
	p.i++
	segs, err := p.parseSegments()
	if err != nil {
		return nil, err
	}
	return &jpSubQuery{abs: abs, segments: segs, singular: segmentsSingular(segs)}, nil
}

func (p *jpParser) parseNumberLiteral() (float64, error) {
	start := p.i
	if p.peek() == '-' {
		p.i++
	}
	ds := p.i
	for !p.eof() && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	digits := p.s[ds:p.i]
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		p.i = start
		return 0, p.errorf("malformed number")
	}
	if p.peek() == '.' {
		p.i++
		fs := p.i
		for !p.eof() && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
		}
		if p.i == fs {
			p.i = start
			return 0, p.errorf("malformed number")
		}
	}
	if c := p.peek(); c == 'e' || c == 'E' {
		p.i++
		if c := p.peek(); c == '+' || c == '-' {
			p.i++
		}
		es := p.i
		for !p.eof() && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
		}
		if p.i == es {
			p.i = start
			return 0, p.errorf("malformed number")
		}
	}
	f, err := strconv.ParseFloat(p.s[start:p.i], 64)
	if err != nil {
		p.i = start
		return 0, p.errorf("malformed number")
	}
	return f, nil
}

// parseWordOperand reads true, false, null, or a function call.
func (p *jpParser) parseWordOperand() (jpOperand, error) {
	start := p.i
	for !p.eof() && (p.s[p.i] >= 'a' && p.s[p.i] <= 'z' || p.s[p.i] >= '0' && p.s[p.i] <= '9' || p.s[p.i] == '_') {
		p.i++
	}
	word := p.s[start:p.i]
	if p.peek() != '(' {
		switch word {
		case "true":
			return jpOperand{lit: &jpLiteral{v: true}}, nil
		case "false":
			return jpOperand{lit: &jpLiteral{v: false}}, nil
		case "null":
			return jpOperand{lit: &jpLiteral{v: nil}}, nil
		}
		p.i = start
		return jpOperand{}, p.errorf("unknown word %q", word)
	}
	if err := p.enter(); err != nil {
		return jpOperand{}, err
	}
	defer p.leave()
	p.i++ // (
	f := &jpFunc{name: word}
	p.skipSpace()
	if !p.consume(")") {
		for {
			p.skipSpace()
			op, err := p.parseOperand()
			if err != nil {
				return jpOperand{}, err
			}
			f.args = append(f.args, jpArg{lit: op.lit, query: op.query, fn: op.fn})
			p.skipSpace()
			if p.consume(",") {
				continue
			}
			if p.consume(")") {
				break
			}
			return jpOperand{}, p.errorf("expected , or ) in the arguments of %s()", word)
		}
	}
	if err := p.checkFunction(f, start); err != nil {
		return jpOperand{}, err
	}
	return jpOperand{fn: f}, nil
}

// checkFunction applies the RFC 9535 type rules for the five built-in functions.
func (p *jpParser) checkFunction(f *jpFunc, at int) error {
	want := map[string]int{"length": 1, "count": 1, "value": 1, "match": 2, "search": 2}
	n, ok := want[f.name]
	if !ok {
		p.i = at
		return p.errorf("unknown function %s()", f.name)
	}
	if len(f.args) != n {
		p.i = at
		return p.errorf("%s() takes %d argument(s), got %d", f.name, n, len(f.args))
	}
	for _, a := range f.args {
		switch f.name {
		case "count", "value":
			// NodesType: a query of any kind.
			if a.query == nil {
				p.i = at
				return p.errorf("%s() takes a query", f.name)
			}
		default:
			// ValueType: a literal, a singular query, or a function that returns a value.
			switch {
			case a.lit != nil:
			case a.query != nil:
				if !a.query.singular {
					p.i = at
					return p.errorf("%s() takes a singular query", f.name)
				}
			case a.fn != nil:
				if jpFuncResultIsLogical(a.fn.name) {
					p.i = at
					return p.errorf("%s() cannot take a logical value", f.name)
				}
			}
		}
	}
	return nil
}
