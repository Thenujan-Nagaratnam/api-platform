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

// Package metrics lets a policy export metrics through the policy engine's
// own metrics endpoint without depending on a metrics library.
//
// The engine installs a Provider at startup with SetProvider. Until then, and
// in unit tests that never call it, every metric is a no-op, so a policy can
// create its metrics unconditionally.
//
// Metric names should be prefixed with the policy's name, and label values
// must come from a bounded set: every distinct combination is a new series.
package metrics

import "sync"

// Counter is a monotonically increasing value per label combination.
type Counter interface {
	// Add increments the series selected by labelValues, given in the order
	// the label names were declared.
	Add(delta float64, labelValues ...string)
}

// Gauge is a value that can go up and down per label combination.
type Gauge interface {
	Set(value float64, labelValues ...string)
}

// Histogram records observations per label combination.
type Histogram interface {
	Observe(value float64, labelValues ...string)
}

// Provider creates metrics. Requesting the same name twice must return a
// metric backed by the same series, so a policy instance re-created on a
// config update keeps counting into the existing metric.
type Provider interface {
	Counter(name, help string, labelNames ...string) Counter
	Gauge(name, help string, labelNames ...string) Gauge
	// Histogram uses the provider's default buckets when buckets is nil.
	Histogram(name, help string, buckets []float64, labelNames ...string) Histogram
}

var (
	mu       sync.RWMutex
	provider Provider = noopProvider{}
)

// SetProvider installs the engine's provider. A nil provider restores the no-op.
func SetProvider(p Provider) {
	mu.Lock()
	defer mu.Unlock()
	if p == nil {
		p = noopProvider{}
	}
	provider = p
}

// NewCounter creates or returns the named counter from the current provider.
func NewCounter(name, help string, labelNames ...string) Counter {
	return current().Counter(name, help, labelNames...)
}

// NewGauge creates or returns the named gauge from the current provider.
func NewGauge(name, help string, labelNames ...string) Gauge {
	return current().Gauge(name, help, labelNames...)
}

// NewHistogram creates or returns the named histogram from the current provider.
func NewHistogram(name, help string, buckets []float64, labelNames ...string) Histogram {
	return current().Histogram(name, help, buckets, labelNames...)
}

func current() Provider {
	mu.RLock()
	defer mu.RUnlock()
	return provider
}

type noopProvider struct{}

func (noopProvider) Counter(string, string, ...string) Counter { return noop{} }
func (noopProvider) Gauge(string, string, ...string) Gauge     { return noop{} }
func (noopProvider) Histogram(string, string, []float64, ...string) Histogram {
	return noop{}
}

type noop struct{}

func (noop) Add(float64, ...string)     {}
func (noop) Set(float64, ...string)     {}
func (noop) Observe(float64, ...string) {}
