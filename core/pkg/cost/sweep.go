package cost

import "time"

// sweep is the running state between two edges.
type sweep struct {
	capacity int64
	known    bool
	total    int64
	busy     int64
	// demand and rate cover only keys with something in flight, so both are
	// bounded by concurrency rather than by the tenant count — which is what
	// keeps a month of traffic sweepable.
	demand map[string]int64
	rate   map[string]int64
	// weighted holds rate×time per key and shares the time units below.
	weighted map[string]int64
}

// hold charges the wall clock between two edges to whoever was asking.
func (s *sweep) hold(from, to time.Time) {
	if s.busy <= 0 || s.total <= 0 || !to.After(from) {
		return
	}
	ms := int64(to.Sub(from) / time.Millisecond)
	if ms <= 0 {
		return
	}
	for k, r := range s.rate {
		// Half the divisor, so the share rounds rather than truncates. Truncating
		// every edge biases all of them the same way, and the losses accumulate
		// into a split that is short of a whole GPU-second every time.
		s.weighted[k] += (r*shareScale + s.total/2) / s.total * ms
	}
}

// add folds one demand change in and re-derives the rates.
//
// A key's rate is its share of a busy instant, not its own demand: halving the
// demand of one of two equally busy keys doubles its share. That is why a
// changed busy figure rebuilds every key rather than only the one that moved.
func (s *sweep) add(key string, delta int64) {
	g := s.demand[key] + delta
	if g > 0 {
		s.demand[key] = g
	} else {
		delete(s.demand, key)
		delete(s.rate, key)
	}
	s.total += delta
	if s.total < 0 {
		s.total = 0
	}
	busy := s.occupied()
	if busy == s.busy {
		s.busy = busy
		if g > 0 {
			s.rate[key] = busy * g
		}
		return
	}
	s.busy = busy
	s.rescaleRates()
}

// rescale re-derives every key's rate after capacity stepped.
func (s *sweep) rescale() {
	busy := s.occupied()
	if busy == s.busy {
		return
	}
	s.busy = busy
	s.rescaleRates()
}

func (s *sweep) rescaleRates() {
	for k, g := range s.demand {
		s.rate[k] = s.busy * g
	}
}

// occupied is how much of the capacity the callers between them asked for.
func (s *sweep) occupied() int64 {
	if !s.known {
		return s.total
	}
	if s.total > s.capacity {
		return s.capacity
	}
	return s.total
}
