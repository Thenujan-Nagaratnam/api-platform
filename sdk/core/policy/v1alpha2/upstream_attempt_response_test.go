/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.com) All Rights Reserved.
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

package policyv1alpha2

import (
	"context"
	"testing"
)

type recordingResponseObserver struct {
	seen []UpstreamAttemptResponseContext
}

func (r *recordingResponseObserver) OnUpstreamAttemptResponse(ctx context.Context, actx *UpstreamAttemptResponseContext) {
	r.seen = append(r.seen, *actx)
}

func TestUpstreamAttemptResponseObserver_Implementable(t *testing.T) {
	var obs UpstreamAttemptResponseObserver = &recordingResponseObserver{}
	obs.OnUpstreamAttemptResponse(context.Background(), &UpstreamAttemptResponseContext{
		AttemptCount:   1,
		RequestID:      "req-abc-123",
		ResponseStatus: 401,
	})
	rec := obs.(*recordingResponseObserver)
	if len(rec.seen) != 1 {
		t.Fatalf("seen = %d entries, want 1", len(rec.seen))
	}
	if rec.seen[0].RequestID != "req-abc-123" || rec.seen[0].ResponseStatus != 401 {
		t.Errorf("unexpected context: %+v", rec.seen[0])
	}
}
