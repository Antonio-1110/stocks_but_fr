package site

import (
	"fmt"
	"sort"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
)

// rankUnpriced fills UnpricedRank and UnpricedScore: the "growth not yet
// priced in" ranking. Each eligible stock gets a percentile rank (0..1, 1 is
// best) on five measures, and its score is their weighted sum (config.Unpriced).
// Low-base outliers, stocks without a price and thin or slow growers are left out.
func rankUnpriced(rows []Row, u config.Unpriced) {
	var idx []int
	for i, r := range rows {
		if r.LowBase || r.LastClose == nil || r.Change1M == nil || r.FromHigh == nil ||
			r.RevYoY == nil || r.RevYoYAvg == nil || r.Turnover20 == nil ||
			*r.RevYoYAvg < u.MinGrowthPct || *r.Turnover20 < u.MinTurnoverNTD {
			continue
		}
		idx = append(idx, i)
	}
	if len(idx) == 0 {
		return
	}

	measures := []struct {
		weight float64
		value  func(Row) float64 // higher is better
	}{
		{u.GrowthWeight, func(r Row) float64 { return *r.RevYoYAvg }},
		{u.AccelWeight, func(r Row) float64 { return *r.RevYoY - *r.RevYoYAvg }},
		{u.PriceWeight, func(r Row) float64 { return -*r.Change1M }},
		{u.HighWeight, func(r Row) float64 { return -*r.FromHigh }},
		{u.FlowWeight, func(r Row) float64 { return -flowShare(r) }},
	}
	total := 0.0
	scores := make([]float64, len(idx))
	for _, m := range measures {
		if m.weight == 0 {
			continue
		}
		total += m.weight
		vals := make([]float64, len(idx))
		for k, i := range idx {
			vals[k] = m.value(rows[i])
		}
		for k, p := range percentiles(vals) {
			scores[k] += m.weight * p
		}
	}
	for k, i := range idx {
		s := scores[k]
		if total > 0 {
			s /= total
		}
		rows[i].UnpricedScore = ptr(s)
	}

	sort.SliceStable(idx, func(a, b int) bool {
		ra, rb := rows[idx[a]], rows[idx[b]]
		if *ra.UnpricedScore != *rb.UnpricedScore {
			return *ra.UnpricedScore > *rb.UnpricedScore
		}
		return ra.Ticker < rb.Ticker
	})
	for n, i := range idx {
		rows[i].UnpricedRank = n + 1
	}
}

// flowShare is 投信+外資 net buying over the flow window as a fraction of the
// value traded in it, so big and small companies compare. No flow data counts as none.
func flowShare(r Row) float64 {
	var net int64
	if r.TrustNet10 != nil {
		net += *r.TrustNet10
	}
	if r.ForeignNet10 != nil {
		net += *r.ForeignNet10
	}
	if *r.Turnover20 <= 0 {
		return 0
	}
	return float64(net) * *r.LastClose / (*r.Turnover20 * flowDays)
}

// percentiles maps each value to its rank among vals scaled to 0..1
// (ties share their average rank); a single value gets 1.
func percentiles(vals []float64) []float64 {
	n := len(vals)
	out := make([]float64, n)
	if n == 1 {
		out[0] = 1
		return out
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return vals[order[a]] < vals[order[b]] })
	for i := 0; i < n; {
		j := i
		for j+1 < n && vals[order[j+1]] == vals[order[i]] {
			j++
		}
		p := float64(i+j) / 2 / float64(n-1)
		for k := i; k <= j; k++ {
			out[order[k]] = p
		}
		i = j + 1
	}
	return out
}

// unpricedNote is the rank cell's tooltip in the "not yet priced in" view.
func unpricedNote(r Row) string {
	if r.UnpricedScore == nil {
		return ""
	}
	return fmt.Sprintf("score %.2f · 10d 投信+外資 net %.1f%% of traded value", *r.UnpricedScore, flowShare(r)*100)
}
