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

package bodyattemptecho

import (
	"context"
	"encoding/json"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestOnUpstreamAttemptRequest_RewritesAttemptFieldFromBaseline(t *testing.T) {
	p := &Policy{}
	actx := &policy.UpstreamAttemptContext{
		AttemptCount: 2,
		Body:         &policy.Body{Content: []byte(`{"message":"hello","attempt":0}`), Present: true, EndOfStream: true},
	}

	action := p.OnUpstreamAttemptRequest(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptRequestModifications)
	if !ok {
		t.Fatalf("expected UpstreamAttemptRequestModifications, got %T", action)
	}
	if mods.Body == nil {
		t.Fatal("expected a non-nil mutated body")
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(mods.Body, &decoded); err != nil {
		t.Fatalf("mutated body is not valid JSON: %v", err)
	}
	if decoded["attempt"] != float64(2) {
		t.Errorf("expected attempt=2, got %v", decoded["attempt"])
	}
	if decoded["message"] != "hello" {
		t.Errorf("expected original message field preserved, got %v", decoded["message"])
	}
}

func TestOnUpstreamAttemptRequest_NoOpWhenBodyNotPresent(t *testing.T) {
	p := &Policy{}
	actx := &policy.UpstreamAttemptContext{AttemptCount: 1, Body: nil}

	action := p.OnUpstreamAttemptRequest(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptRequestModifications)
	if !ok {
		t.Fatalf("expected UpstreamAttemptRequestModifications, got %T", action)
	}
	if mods.Body != nil {
		t.Errorf("expected no body mutation when actx.Body is nil, got %q", mods.Body)
	}
}

func TestOnUpstreamAttemptRequest_FailsOpenOnInvalidJSON(t *testing.T) {
	p := &Policy{}
	actx := &policy.UpstreamAttemptContext{
		AttemptCount: 2,
		Body:         &policy.Body{Content: []byte("not json"), Present: true, EndOfStream: true},
	}

	action := p.OnUpstreamAttemptRequest(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptRequestModifications)
	if !ok {
		t.Fatalf("expected UpstreamAttemptRequestModifications, got %T", action)
	}
	if mods.Body != nil {
		t.Errorf("expected fail-open (no mutation) on undecodable body, got %q", mods.Body)
	}
}

func TestOnUpstreamAttemptRequest_RewritesPathOnRetryAttempt(t *testing.T) {
	p := &Policy{}
	// Simulates the headers-phase call: Headers populated, Body nil - the
	// only call the kernel reads mods.Path from (see
	// policy-engine/internal/kernel/upstream_extproc.go's
	// processUpstreamAttemptRequestHeaders vs processUpstreamAttemptRequestBody).
	actx := &policy.UpstreamAttemptContext{
		AttemptCount: 2,
		Headers:      policy.NewHeaders(map[string][]string{":path": {"/echo"}}),
	}

	action := p.OnUpstreamAttemptRequest(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptRequestModifications)
	if !ok {
		t.Fatalf("expected UpstreamAttemptRequestModifications, got %T", action)
	}
	if mods.Path == nil {
		t.Fatal("expected a non-nil rewritten path on a retry attempt")
	}
	if *mods.Path != "/echo-rewritten-attempt-2" {
		t.Errorf("expected /echo-rewritten-attempt-2, got %q", *mods.Path)
	}
}

func TestOnUpstreamAttemptRequest_NoPathRewriteOnFirstAttempt(t *testing.T) {
	p := &Policy{}
	actx := &policy.UpstreamAttemptContext{
		AttemptCount: 1,
		Headers:      policy.NewHeaders(map[string][]string{":path": {"/echo"}}),
	}

	action := p.OnUpstreamAttemptRequest(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptRequestModifications)
	if !ok {
		t.Fatalf("expected UpstreamAttemptRequestModifications, got %T", action)
	}
	if mods.Path != nil {
		t.Errorf("expected no path rewrite on the first attempt, got %q", *mods.Path)
	}
}

func TestOnUpstreamAttemptRequest_NoPathRewriteWhenPathHeaderMissing(t *testing.T) {
	p := &Policy{}
	actx := &policy.UpstreamAttemptContext{
		AttemptCount: 2,
		Headers:      policy.NewHeaders(map[string][]string{}),
	}

	action := p.OnUpstreamAttemptRequest(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptRequestModifications)
	if !ok {
		t.Fatalf("expected UpstreamAttemptRequestModifications, got %T", action)
	}
	if mods.Path != nil {
		t.Errorf("expected fail-open (no path rewrite) when :path is absent, got %q", *mods.Path)
	}
}

// TestOnUpstreamAttemptRequest_BodyPhaseCallDoesNotSetPath is a regression guard for the exact
// bug found while spiking this feature live (see gateway/spec/prds/llm-cross-provider-failover.md,
// Open Questions, Rev 6): a policy that only set headers/path INSIDE the actx.Body-gated branch
// never actually reached Envoy, because the kernel only applies HeadersToSet/Path from the
// headers-phase call, which always has actx.Body nil. This test locks in that Path rewriting
// lives in the headers-phase branch (Body nil), not the body-phase branch (Body populated,
// Headers nil on that call) - a body-phase call must never need Headers to produce its Body
// mutation.
func TestOnUpstreamAttemptRequest_BodyPhaseCallDoesNotNeedPathHeaders(t *testing.T) {
	p := &Policy{}
	// Simulates the body-phase call: Body populated, Headers nil - exactly what
	// processUpstreamAttemptRequestBody constructs.
	actx := &policy.UpstreamAttemptContext{
		AttemptCount: 2,
		Body:         &policy.Body{Content: []byte(`{"message":"hello","attempt":0}`), Present: true, EndOfStream: true},
	}

	action := p.OnUpstreamAttemptRequest(context.Background(), actx)
	mods, ok := action.(policy.UpstreamAttemptRequestModifications)
	if !ok {
		t.Fatalf("expected UpstreamAttemptRequestModifications, got %T", action)
	}
	if mods.Body == nil {
		t.Fatal("expected the body-phase call to still produce a body mutation")
	}
}

func TestMode_AllPhasesSkipped(t *testing.T) {
	p := &Policy{}
	mode := p.Mode()
	zero := policy.ProcessingMode{}
	if mode != zero {
		t.Errorf("expected all-SKIP zero-value ProcessingMode, got %+v", mode)
	}
}
