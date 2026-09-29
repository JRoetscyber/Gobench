package main

import (
	"math"
	"time"
)

// A fixed-size log-scale histogram: constant memory no matter how long the run,
// mergeable across workers, ~2% percentile precision.
const (
	histMin     = time.Microsecond
	histGrowth  = 1.02
	histBuckets = 1100 // covers 1us .. ~2800s
)

var logGrowth = math.Log(histGrowth)

type histogram struct {
	counts   [histBuckets]uint32
	total    uint64
	sum      time.Duration
	sumSq    float64 // sum of squares in ns^2, for the standard deviation
	min, max time.Duration
}

func bucketIndex(d time.Duration) int {
	if d <= histMin {
		return 0
	}
	i := int(math.Log(float64(d)/float64(histMin))/logGrowth) + 1
	if i >= histBuckets {
		i = histBuckets - 1
	}
	return i
}

func bucketUpper(i int) time.Duration {
	return time.Duration(float64(histMin) * math.Pow(histGrowth, float64(i)))
}

func (h *histogram) add(d time.Duration) {
	if h.total == 0 || d < h.min {
		h.min = d
	}
	if d > h.max {
		h.max = d
	}
	h.counts[bucketIndex(d)]++
	h.total++
	h.sum += d
	f := float64(d)
	h.sumSq += f * f
}

func (h *histogram) merge(o *histogram) {
	if o.total == 0 {
		return
	}
	if h.total == 0 || o.min < h.min {
		h.min = o.min
	}
	if o.max > h.max {
		h.max = o.max
	}
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.total += o.total
	h.sum += o.sum
	h.sumSq += o.sumSq
}

func (h *histogram) mean() time.Duration {
	if h.total == 0 {
		return 0
	}
	return h.sum / time.Duration(h.total)
}

func (h *histogram) stddev() time.Duration {
	if h.total < 2 {
		return 0
	}
	n := float64(h.total)
	mean := float64(h.sum) / n
	v := h.sumSq/n - mean*mean
	if v < 0 {
		v = 0
	}
	return time.Duration(math.Sqrt(v))
}

func (h *histogram) percentile(p float64) time.Duration {
	if h.total == 0 {
		return 0
	}
	target := uint64(math.Ceil(p * float64(h.total)))
	if target < 1 {
		target = 1
	}
	var cum uint64
	for i, c := range h.counts {
		cum += uint64(c)
		if cum >= target {
			v := bucketUpper(i)
			if v < h.min {
				v = h.min
			}
			if v > h.max {
				v = h.max
			}
			return v
		}
	}
	return h.max
}

type bin struct {
	lo, hi time.Duration
	count  uint64
}

// bins groups the histogram into n log-spaced ranges between min and max,
// for drawing the latency distribution.
func (h *histogram) bins(n int) []bin {
	if h.total == 0 || n < 1 {
		return nil
	}
	if h.max <= h.min {
		return []bin{{h.min, h.max, h.total}}
	}
	lo, hi := float64(h.min), float64(h.max)
	if lo < 1 {
		lo = 1
	}
	ratio := math.Pow(hi/lo, 1/float64(n))
	out := make([]bin, n)
	edge := lo
	for i := range out {
		out[i].lo = time.Duration(edge)
		edge *= ratio
		out[i].hi = time.Duration(edge)
	}
	out[n-1].hi = h.max
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		u := bucketUpper(i)
		if u < h.min {
			u = h.min
		}
		if u > h.max {
			u = h.max
		}
		j := 0
		for j < n-1 && float64(u) > float64(out[j].hi) {
			j++
		}
		out[j].count += uint64(c)
	}
	return out
}
