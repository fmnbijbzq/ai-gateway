package balancer

import (
	"sync"

	"ai-gateway/internal/upstream"
)

// Balancer selects an upstream for each request using weighted round-robin
// with least-connections fallback.
type Balancer struct {
	upstreams []*upstream.Upstream

	mu      sync.Mutex
	current int
	gcd     int
	maxW    int
	cw      int // current weight for WRR
}

// New creates a Balancer from the given upstreams.
func New(upstreams []*upstream.Upstream) *Balancer {
	b := &Balancer{
		upstreams: upstreams,
		current:   -1,
	}
	b.gcd = b.computeGCD()
	b.maxW = b.computeMaxWeight()
	return b
}

// Select picks the best available upstream that supports the given model.
// It uses weighted round-robin as primary strategy and falls back to
// least-connections if WRR yields no result.
func (b *Balancer) Select(model string) *upstream.Upstream {
	candidates := b.availableCandidates(model)
	if len(candidates) == 0 {
		return nil
	}

	// Try weighted round-robin first
	if u := b.weightedRoundRobin(candidates); u != nil {
		return u
	}

	// Fallback: least connections
	return b.leastConnections(candidates)
}

func (b *Balancer) availableCandidates(model string) []*upstream.Upstream {
	var result []*upstream.Upstream
	for _, u := range b.upstreams {
		if u.IsAvailable() && u.SupportsModel(model) {
			result = append(result, u)
		}
	}
	return result
}

func (b *Balancer) weightedRoundRobin(candidates []*upstream.Upstream) *upstream.Upstream {
	if len(candidates) == 0 {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(candidates)
	// Run through at most n full cycles to find a candidate
	for i := 0; i < n*b.maxW; i++ {
		b.current = (b.current + 1) % n
		if b.current == 0 {
			b.cw -= b.gcd
			if b.cw <= 0 {
				b.cw = b.maxW
			}
		}
		if candidates[b.current].Weight >= b.cw {
			return candidates[b.current]
		}
	}
	// Should not happen, but return first candidate as fallback
	return candidates[0]
}

func (b *Balancer) leastConnections(candidates []*upstream.Upstream) *upstream.Upstream {
	if len(candidates) == 0 {
		return nil
	}

	best := candidates[0]
	bestConns := best.ActiveConns()

	for _, u := range candidates[1:] {
		conns := u.ActiveConns()
		if conns < bestConns {
			best = u
			bestConns = conns
		}
	}
	return best
}

func (b *Balancer) computeGCD() int {
	if len(b.upstreams) == 0 {
		return 1
	}
	result := b.upstreams[0].Weight
	for _, u := range b.upstreams[1:] {
		result = gcd(result, u.Weight)
	}
	if result == 0 {
		return 1
	}
	return result
}

func (b *Balancer) computeMaxWeight() int {
	max := 0
	for _, u := range b.upstreams {
		if u.Weight > max {
			max = u.Weight
		}
	}
	if max == 0 {
		return 1
	}
	return max
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Upstreams returns all managed upstreams.
func (b *Balancer) Upstreams() []*upstream.Upstream {
	return b.upstreams
}
