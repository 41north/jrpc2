// Copyright (C) 2017 Michael J. Fromberger. All Rights Reserved.

// Package metrics defines a concurrently-accessible metrics collector.
//
// A *metrics.M value exports methods to track integer counters and maximum
// values. A metric has a caller-assigned string name that is not interpreted
// by the collector except to locate its stored value.
package metrics

import "sync"

// An M collects counters and maximum value trackers.  A nil *M is valid, and
// discards all metrics. The methods of an *M are safe for concurrent use by
// multiple goroutines.
type M struct {
	mu      sync.Mutex
	counter map[string]int64
	maxVal  map[string]int64
	label   map[string]interface{}
}

// New creates a new, empty metrics collector.
func New() *M {
	return &M{
		counter: make(map[string]int64),
		maxVal:  make(map[string]int64),
		label:   make(map[string]interface{}),
	}
}

// Count adds n to the current value of the counter named, defining the counter
// if it does not already exist.
func (m *M) Count(name string, n int64) {
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.counter[name] += n
	}
}

// SetMaxValue sets the maximum value metric named to the greater of n and its
// current value, defining the value if it does not already exist.
func (m *M) SetMaxValue(name string, n int64) {
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if old, ok := m.maxVal[name]; !ok || n > old {
			m.maxVal[name] = n
		}
	}
}

// CountAndSetMax adds n to the current value of the counter named, and also
// updates a max value tracker with the same name in a single step.
func (m *M) CountAndSetMax(name string, n int64) {
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if old, ok := m.maxVal[name]; !ok || n > old {
			m.maxVal[name] = n
		}
		m.counter[name] += n
	}
}

// SetLabel sets the specified label to value. If value == nil the label is
// removed from the set.
//
// As a special case, if value has the concrete type
//
//	func() interface{}
//
// then the value of the label is obtained by calling that function.
func (m *M) SetLabel(name string, value interface{}) {
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if value == nil {
			delete(m.label, name)
		} else {
			m.label[name] = value
		}
	}
}

// EditLabel calls edit with the current value of the specified label.
// The value returned by edit replaces the contents of the label.
// If edit returns nil, the label is removed from the set.
// If the label did not exist, the argument to edit is nil.
func (m *M) EditLabel(name string, edit func(interface{}) interface{}) {
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		newValue := edit(m.label[name])
		if newValue == nil {
			delete(m.label, name)
		} else {
			m.label[name] = newValue
		}
	}
}

// Snapshot copies an atomic snapshot of the collected metrics into the non-nil
// fields of the provided snapshot value. Only the fields of snap that are not
// nil are snapshotted.
func (m *M) Snapshot(snap Snapshot) {
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if c := snap.Counter; c != nil {
			for name, val := range m.counter {
				c[name] = val
			}
		}
		if v := snap.MaxValue; v != nil {
			for name, val := range m.maxVal {
				v[name] = val
			}
		}
		if v := snap.Label; v != nil {
			for name, val := range m.label {
				if fn, ok := val.(func() interface{}); ok {
					v[name] = fn()
				} else {
					v[name] = val
				}
			}
		}
	}
}

// A Snapshot represents a point-in-time snapshot of a metrics collector.  The
// fields of this type are filled in by the Snapshot method of *M.
type Snapshot struct {
	Counter  map[string]int64
	MaxValue map[string]int64
	Label    map[string]interface{}
}
