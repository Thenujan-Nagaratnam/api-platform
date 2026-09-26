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

package metrics

import (
	"sync"
	"testing"
)

type recordingProvider struct {
	mu    sync.Mutex
	added map[string]float64
}

func (p *recordingProvider) Counter(name, _ string, _ ...string) Counter {
	return recordingCounter{p: p, name: name}
}
func (p *recordingProvider) Gauge(string, string, ...string) Gauge { return noop{} }
func (p *recordingProvider) Histogram(string, string, []float64, ...string) Histogram {
	return noop{}
}

type recordingCounter struct {
	p    *recordingProvider
	name string
}

func (c recordingCounter) Add(delta float64, _ ...string) {
	c.p.mu.Lock()
	c.p.added[c.name] += delta
	c.p.mu.Unlock()
}

func TestNoopBeforeProviderIsSet(t *testing.T) {
	SetProvider(nil)
	// Must not panic, and must accept any label arity.
	NewCounter("c", "h", "a").Add(1, "x")
	NewGauge("g", "h").Set(2)
	NewHistogram("h", "h", nil, "a", "b").Observe(3, "x", "y")
}

func TestProviderReceivesMetrics(t *testing.T) {
	p := &recordingProvider{added: map[string]float64{}}
	SetProvider(p)
	defer SetProvider(nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			NewCounter("requests_total", "h", "route").Add(1, "r")
		}()
	}
	wg.Wait()
	if got := p.added["requests_total"]; got != 50 {
		t.Fatalf("expected 50, got %v", got)
	}
}
