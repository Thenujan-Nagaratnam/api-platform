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
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SelectNormalizedPaths evaluates an RFC 9535 JSONPath expression against a JSON
// document and returns the normalized path of every matched node, in document
// order, for example "$['messages'][0]['content'][1]['text']".
//
// The evaluator is read-only. It never changes the document and never
// re-serializes it, so a caller can use the returned paths to rewrite selected
// values in the original bytes without touching anything else.
//
// Supported: child and descendant segments, name, wildcard, index (negative
// allowed), slice, union of selectors, filter selectors with comparison,
// existence tests, && || ! and parentheses, and the length, count, match,
// search and value functions.
func SelectNormalizedPaths(payload []byte, expr string) ([]string, error) {
	var doc interface{}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid JSON document: %w", err)
	}
	nodes, err := SelectNodes(doc, expr)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(nodes))
	for i, n := range nodes {
		paths[i] = n.Path
	}
	return paths, nil
}

// JSONNode is a node selected by a JSONPath query.
type JSONNode struct {
	Path  string
	Value interface{}
}

// SelectNodes evaluates expr against doc, which must come from a decoder that
// used json.Number (or plain float64) for numbers.
func SelectNodes(doc interface{}, expr string) ([]JSONNode, error) {
	q, err := parseJSONPathQuery(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid JSONPath %q: %w", expr, err)
	}
	return q.run(doc, JSONNode{Path: "$", Value: doc}), nil
}

// ───────────────────────── parsing ─────────────────────────

type jpSegment struct {
	descendant bool
	selectors  []jpSelector
}

type jpSelector struct {
	kind   int // selName, selIndex, selWildcard, selSlice, selFilter
	name   string
	index  int
	start  *int
	end    *int
	step   int
	filter jpExpr
}

const (
	selName = iota
	selIndex
	selWildcard
	selSlice
	selFilter
)

type jpQuery struct {
	root     bool // $ (true) or @ (false)
	segments []jpSegment
}

type jpParser struct {
	s   string
	pos int
}

func parseJSONPathQuery(expr string) (*jpQuery, error) {
	p := &jpParser{s: expr}
	p.skipWS()
	q, err := p.query()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos != len(p.s) {
		return nil, fmt.Errorf("unexpected %q at offset %d", p.s[p.pos:], p.pos)
	}
	if !q.root {
		return nil, fmt.Errorf("expression must start with $")
	}
	return q, nil
}

func (p *jpParser) skipWS() {
	for p.pos < len(p.s) && strings.IndexByte(" \t\r\n", p.s[p.pos]) >= 0 {
		p.pos++
	}
}

func (p *jpParser) peek() byte {
	if p.pos < len(p.s) {
		return p.s[p.pos]
	}
	return 0
}

func (p *jpParser) lookingAt(prefix string) bool { return strings.HasPrefix(p.s[p.pos:], prefix) }

func (p *jpParser) query() (*jpQuery, error) {
	q := &jpQuery{}
	switch p.peek() {
	case '$':
		q.root = true
	case '@':
	default:
		return nil, fmt.Errorf("expected $ or @ at offset %d", p.pos)
	}
	p.pos++
	for {
		save := p.pos
		p.skipWS()
		switch {
		case p.lookingAt(".."):
			p.pos += 2
			seg := jpSegment{descendant: true}
			switch {
			case p.peek() == '[':
				sels, err := p.bracket()
				if err != nil {
					return nil, err
				}
				seg.selectors = sels
			case p.peek() == '*':
				p.pos++
				seg.selectors = []jpSelector{{kind: selWildcard}}
			default:
				name, err := p.memberName()
				if err != nil {
					return nil, err
				}
				seg.selectors = []jpSelector{{kind: selName, name: name}}
			}
			q.segments = append(q.segments, seg)
		case p.peek() == '.':
			p.pos++
			seg := jpSegment{}
			if p.peek() == '*' {
				p.pos++
				seg.selectors = []jpSelector{{kind: selWildcard}}
			} else {
				name, err := p.memberName()
				if err != nil {
					return nil, err
				}
				seg.selectors = []jpSelector{{kind: selName, name: name}}
			}
			q.segments = append(q.segments, seg)
		case p.peek() == '[':
			sels, err := p.bracket()
			if err != nil {
				return nil, err
			}
			q.segments = append(q.segments, jpSegment{selectors: sels})
		default:
			p.pos = save
			return q, nil
		}
	}
}

func isNameFirst(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r >= 0x80
}

func isNameChar(r rune) bool { return isNameFirst(r) || (r >= '0' && r <= '9') }

func (p *jpParser) memberName() (string, error) {
	start := p.pos
	for p.pos < len(p.s) {
		r, size := utf8.DecodeRuneInString(p.s[p.pos:])
		if p.pos == start && !isNameFirst(r) {
			break
		}
		if !isNameChar(r) {
			break
		}
		p.pos += size
	}
	if p.pos == start {
		return "", fmt.Errorf("expected member name at offset %d", p.pos)
	}
	return p.s[start:p.pos], nil
}

func (p *jpParser) bracket() ([]jpSelector, error) {
	p.pos++ // [
	var sels []jpSelector
	for {
		p.skipWS()
		sel, err := p.selector()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
		p.skipWS()
		switch p.peek() {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return sels, nil
		default:
			return nil, fmt.Errorf("expected ',' or ']' at offset %d", p.pos)
		}
	}
}

func (p *jpParser) selector() (jpSelector, error) {
	c := p.peek()
	switch {
	case c == '\'' || c == '"':
		name, err := p.quoted()
		return jpSelector{kind: selName, name: name}, err
	case c == '*':
		p.pos++
		return jpSelector{kind: selWildcard}, nil
	case c == '?':
		p.pos++
		p.skipWS()
		e, err := p.orExpr()
		return jpSelector{kind: selFilter, filter: e}, err
	default:
		return p.indexOrSlice()
	}
}

func (p *jpParser) intLiteral() (*int, error) {
	start := p.pos
	if p.peek() == '-' {
		p.pos++
	}
	for p.pos < len(p.s) && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == start || p.s[start:p.pos] == "-" {
		p.pos = start
		return nil, nil
	}
	n, err := strconv.Atoi(p.s[start:p.pos])
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (p *jpParser) indexOrSlice() (jpSelector, error) {
	first, err := p.intLiteral()
	if err != nil {
		return jpSelector{}, err
	}
	p.skipWS()
	if p.peek() != ':' {
		if first == nil {
			return jpSelector{}, fmt.Errorf("expected selector at offset %d", p.pos)
		}
		return jpSelector{kind: selIndex, index: *first}, nil
	}
	p.pos++
	p.skipWS()
	end, err := p.intLiteral()
	if err != nil {
		return jpSelector{}, err
	}
	p.skipWS()
	step := 1
	if p.peek() == ':' {
		p.pos++
		p.skipWS()
		st, err := p.intLiteral()
		if err != nil {
			return jpSelector{}, err
		}
		if st != nil {
			step = *st
		}
	}
	return jpSelector{kind: selSlice, start: first, end: end, step: step}, nil
}

func (p *jpParser) quoted() (string, error) {
	quote := p.s[p.pos]
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == quote {
			p.pos++
			return sb.String(), nil
		}
		if c == '\\' && p.pos+1 < len(p.s) {
			p.pos++
			switch e := p.s[p.pos]; e {
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case '/', '\\', '\'', '"':
				sb.WriteByte(e)
			case 'u':
				if p.pos+4 >= len(p.s) {
					return "", fmt.Errorf("bad \\u escape")
				}
				n, err := strconv.ParseUint(p.s[p.pos+1:p.pos+5], 16, 32)
				if err != nil {
					return "", fmt.Errorf("bad \\u escape")
				}
				sb.WriteRune(rune(n))
				p.pos += 4
			default:
				return "", fmt.Errorf("bad escape \\%c", e)
			}
			p.pos++
			continue
		}
		sb.WriteByte(c)
		p.pos++
	}
	return "", fmt.Errorf("unterminated string literal")
}

// ── filter expressions ──

type jpExpr interface{ eval(ctx *jpCtx) jpValue }

type jpCtx struct{ root, current interface{} }

// jpValue is the result of a filter expression: a logical value, a comparable
// value, a node list (from a query), or "nothing".
type jpValue struct {
	kind  int // vNothing, vValue, vBool, vNodes
	value interface{}
	b     bool
	nodes []JSONNode
}

const (
	vNothing = iota
	vValue
	vBool
	vNodes
)

func (p *jpParser) orExpr() (jpExpr, error) {
	left, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		if !p.lookingAt("||") {
			return left, nil
		}
		p.pos += 2
		right, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		l, r := left, right
		left = jpFunc(func(c *jpCtx) jpValue { return jpBool(truthy(l.eval(c)) || truthy(r.eval(c))) })
	}
}

func (p *jpParser) andExpr() (jpExpr, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		if !p.lookingAt("&&") {
			return left, nil
		}
		p.pos += 2
		right, err := p.unary()
		if err != nil {
			return nil, err
		}
		l, r := left, right
		left = jpFunc(func(c *jpCtx) jpValue { return jpBool(truthy(l.eval(c)) && truthy(r.eval(c))) })
	}
}

type jpFunc func(*jpCtx) jpValue

func (f jpFunc) eval(c *jpCtx) jpValue { return f(c) }

func jpBool(b bool) jpValue { return jpValue{kind: vBool, b: b} }

func truthy(v jpValue) bool {
	switch v.kind {
	case vBool:
		return v.b
	case vNodes:
		return len(v.nodes) > 0
	default:
		return false
	}
}

func (p *jpParser) unary() (jpExpr, error) {
	p.skipWS()
	if p.peek() == '!' && !p.lookingAt("!=") {
		p.pos++
		inner, err := p.unary()
		if err != nil {
			return nil, err
		}
		return jpFunc(func(c *jpCtx) jpValue { return jpBool(!truthy(inner.eval(c))) }), nil
	}
	if p.peek() == '(' {
		p.pos++
		inner, err := p.orExpr()
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.peek() != ')' {
			return nil, fmt.Errorf("expected ')' at offset %d", p.pos)
		}
		p.pos++
		return inner, nil
	}
	left, err := p.operand()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if p.lookingAt(op) {
			p.pos += len(op)
			p.skipWS()
			right, err := p.operand()
			if err != nil {
				return nil, err
			}
			o := op
			return jpFunc(func(c *jpCtx) jpValue {
				return jpBool(compareValues(o, singular(left.eval(c)), singular(right.eval(c))))
			}), nil
		}
	}
	return left, nil
}

func (p *jpParser) operand() (jpExpr, error) {
	p.skipWS()
	c := p.peek()
	switch {
	case c == '\'' || c == '"':
		s, err := p.quoted()
		return literal(s), err
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.pos
		p.pos++
		for p.pos < len(p.s) && strings.IndexByte("0123456789.eE+-", p.s[p.pos]) >= 0 {
			p.pos++
		}
		f, err := strconv.ParseFloat(p.s[start:p.pos], 64)
		if err != nil {
			return nil, fmt.Errorf("bad number %q", p.s[start:p.pos])
		}
		return literal(f), nil
	case c == '@' || c == '$':
		q, err := p.query()
		if err != nil {
			return nil, err
		}
		return jpFunc(func(ctx *jpCtx) jpValue {
			start := ctx.current
			if q.root {
				start = ctx.root
			}
			return jpValue{kind: vNodes, nodes: q.run(ctx.root, JSONNode{Path: "", Value: start})}
		}), nil
	case p.lookingAt("true"):
		p.pos += 4
		return literal(true), nil
	case p.lookingAt("false"):
		p.pos += 5
		return literal(false), nil
	case p.lookingAt("null"):
		p.pos += 4
		return literal(nil), nil
	case isNameFirst(rune(c)):
		return p.function()
	}
	return nil, fmt.Errorf("unexpected %q at offset %d", string(c), p.pos)
}

func literal(v interface{}) jpExpr {
	return jpFunc(func(*jpCtx) jpValue { return jpValue{kind: vValue, value: v} })
}

func (p *jpParser) function() (jpExpr, error) {
	name, err := p.memberName()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.peek() != '(' {
		return nil, fmt.Errorf("unknown identifier %q", name)
	}
	p.pos++
	var args []jpExpr
	for {
		p.skipWS()
		if p.peek() == ')' {
			p.pos++
			break
		}
		a, err := p.orExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		p.skipWS()
		if p.peek() == ',' {
			p.pos++
		}
	}
	want := map[string]int{"length": 1, "count": 1, "value": 1, "match": 2, "search": 2}
	n, ok := want[name]
	if !ok {
		return nil, fmt.Errorf("unknown function %q", name)
	}
	if len(args) != n {
		return nil, fmt.Errorf("function %s expects %d argument(s)", name, n)
	}
	return jpFunc(func(c *jpCtx) jpValue { return callFunction(name, args, c) }), nil
}

func callFunction(name string, args []jpExpr, c *jpCtx) jpValue {
	switch name {
	case "length":
		v := singular(args[0].eval(c))
		switch t := v.value.(type) {
		case string:
			return jpValue{kind: vValue, value: float64(utf8.RuneCountInString(t))}
		case []interface{}:
			return jpValue{kind: vValue, value: float64(len(t))}
		case map[string]interface{}:
			return jpValue{kind: vValue, value: float64(len(t))}
		}
		return jpValue{kind: vNothing}
	case "count":
		v := args[0].eval(c)
		if v.kind != vNodes {
			return jpValue{kind: vNothing}
		}
		return jpValue{kind: vValue, value: float64(len(v.nodes))}
	case "value":
		v := args[0].eval(c)
		if v.kind == vNodes && len(v.nodes) == 1 {
			return jpValue{kind: vValue, value: v.nodes[0].Value}
		}
		return jpValue{kind: vNothing}
	default: // match, search
		s, ok1 := singular(args[0].eval(c)).value.(string)
		pat, ok2 := singular(args[1].eval(c)).value.(string)
		if !ok1 || !ok2 {
			return jpBool(false)
		}
		if name == "match" {
			pat = "^(?:" + pat + ")$"
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return jpBool(false)
		}
		return jpBool(re.MatchString(s))
	}
}

// singular reduces a query result to a comparable value: a query with exactly
// one node is that node's value, anything else is "nothing".
func singular(v jpValue) jpValue {
	if v.kind == vNodes {
		if len(v.nodes) == 1 {
			return jpValue{kind: vValue, value: v.nodes[0].Value}
		}
		return jpValue{kind: vNothing}
	}
	return v
}

func asFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	}
	return 0, false
}

func compareValues(op string, a, b jpValue) bool {
	if a.kind == vNothing || b.kind == vNothing {
		if a.kind == vNothing && b.kind == vNothing {
			return op == "==" || op == "<=" || op == ">="
		}
		return op == "!="
	}
	eq := valuesEqual(a.value, b.value)
	switch op {
	case "==":
		return eq
	case "!=":
		return !eq
	}
	if x, ok := asFloat(a.value); ok {
		if y, ok := asFloat(b.value); ok {
			switch op {
			case "<":
				return x < y
			case "<=":
				return x <= y
			case ">":
				return x > y
			case ">=":
				return x >= y
			}
		}
	}
	if x, ok := a.value.(string); ok {
		if y, ok := b.value.(string); ok {
			switch op {
			case "<":
				return x < y
			case "<=":
				return x <= y
			case ">":
				return x > y
			case ">=":
				return x >= y
			}
		}
	}
	return false
}

func valuesEqual(a, b interface{}) bool {
	if x, ok := asFloat(a); ok {
		if y, ok := asFloat(b); ok {
			return x == y
		}
		return false
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case nil:
		return b == nil
	}
	return false
}

// ───────────────────────── evaluation ─────────────────────────

func (q *jpQuery) run(root interface{}, start JSONNode) []JSONNode {
	current := []JSONNode{start}
	for _, seg := range q.segments {
		var next []JSONNode
		for _, n := range current {
			inputs := []JSONNode{n}
			if seg.descendant {
				inputs = descendantsOrSelf(n)
			}
			for _, in := range inputs {
				for _, sel := range seg.selectors {
					next = append(next, applySelector(sel, root, in)...)
				}
			}
		}
		current = next
	}
	return current
}

// descendantsOrSelf returns n and all of its descendants in document order.
func descendantsOrSelf(n JSONNode) []JSONNode {
	out := []JSONNode{n}
	for _, c := range children(n) {
		out = append(out, descendantsOrSelf(c)...)
	}
	return out
}

func children(n JSONNode) []JSONNode {
	switch t := n.Value.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]JSONNode, 0, len(keys))
		for _, k := range keys {
			out = append(out, JSONNode{Path: n.Path + "['" + escapeNormalizedPathName(k) + "']", Value: t[k]})
		}
		return out
	case []interface{}:
		out := make([]JSONNode, 0, len(t))
		for i, v := range t {
			out = append(out, JSONNode{Path: n.Path + "[" + strconv.Itoa(i) + "]", Value: v})
		}
		return out
	}
	return nil
}

func applySelector(sel jpSelector, root interface{}, n JSONNode) []JSONNode {
	switch sel.kind {
	case selName:
		if m, ok := n.Value.(map[string]interface{}); ok {
			if v, ok := m[sel.name]; ok {
				return []JSONNode{{Path: n.Path + "['" + escapeNormalizedPathName(sel.name) + "']", Value: v}}
			}
		}
	case selIndex:
		if a, ok := n.Value.([]interface{}); ok {
			i := sel.index
			if i < 0 {
				i += len(a)
			}
			if i >= 0 && i < len(a) {
				return []JSONNode{{Path: n.Path + "[" + strconv.Itoa(i) + "]", Value: a[i]}}
			}
		}
	case selWildcard:
		return children(n)
	case selSlice:
		if a, ok := n.Value.([]interface{}); ok {
			return slice(n, a, sel)
		}
	case selFilter:
		var out []JSONNode
		for _, c := range children(n) {
			ctx := &jpCtx{root: root, current: c.Value}
			if truthy(sel.filter.eval(ctx)) {
				out = append(out, c)
			}
		}
		return out
	}
	return nil
}

func slice(n JSONNode, a []interface{}, sel jpSelector) []JSONNode {
	if sel.step == 0 {
		return nil
	}
	length := len(a)
	norm := func(i int) int {
		if i >= 0 {
			return i
		}
		return length + i
	}
	var lo, hi int
	if sel.step > 0 {
		lo, hi = 0, length
		if sel.start != nil {
			lo = norm(*sel.start)
		}
		if sel.end != nil {
			hi = norm(*sel.end)
		}
		lo, hi = clamp(lo, 0, length), clamp(hi, 0, length)
		var out []JSONNode
		for i := lo; i < hi; i += sel.step {
			out = append(out, JSONNode{Path: n.Path + "[" + strconv.Itoa(i) + "]", Value: a[i]})
		}
		return out
	}
	lo, hi = length-1, -1
	if sel.start != nil {
		lo = norm(*sel.start)
	}
	if sel.end != nil {
		hi = norm(*sel.end)
	}
	lo, hi = clamp(lo, -1, length-1), clamp(hi, -1, length-1)
	var out []JSONNode
	for i := lo; i > hi; i += sel.step {
		out = append(out, JSONNode{Path: n.Path + "[" + strconv.Itoa(i) + "]", Value: a[i]})
	}
	return out
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// escapeNormalizedPathName escapes a member name for the single-quoted form of
// an RFC 9535 normalized path.
func escapeNormalizedPathName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r == '\'':
			sb.WriteString(`\'`)
		case r == '\\':
			sb.WriteString(`\\`)
		case r == '\b':
			sb.WriteString(`\b`)
		case r == '\f':
			sb.WriteString(`\f`)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r == '\t':
			sb.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
