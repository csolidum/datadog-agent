// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2020 Datadog, Inc.

package stats

import (
	"fmt"
	"sort"

	"github.com/DataDog/datadog-agent/pkg/trace/pb"
	"github.com/DataDog/datadog-agent/pkg/trace/traceutil"
)

// SublayerValue is just a span-metric placeholder for a given sublayer val
type SublayerValue struct {
	Metric string
	Tag    Tag
	Value  float64
}

// String returns a description of a sublayer value.
func (v SublayerValue) String() string {
	if v.Tag.Name == "" && v.Tag.Value == "" {
		return fmt.Sprintf("SublayerValue{%q, %v}", v.Metric, v.Value)
	}

	return fmt.Sprintf("SublayerValue{%q, %v, %v}", v.Metric, v.Tag, v.Value)
}

// GoString returns a description of a sublayer value.
func (v SublayerValue) GoString() string {
	return v.String()
}

// ComputeSublayers extracts sublayer values by type and service for a trace
//
// Description of the algorithm, with the following trace as an example:
//
// 0  10  20  30  40  50  60  70  80  90 100 110 120 130 140 150
// |===|===|===|===|===|===|===|===|===|===|===|===|===|===|===|
// <-1------------------------------------------------->
//     <-2----------------->       <-3--------->
//         <-4--------->
//       <-5------------------->
//                         <--6-------------------->
//                                             <-7------------->
// 1: service=web-server, type=web,   parent=nil
// 2: service=pg,         type=db,    parent=1
// 3: service=render,     type=web,   parent=1
// 4: service=pg-read,    type=db,    parent=2
// 5: service=redis,      type=cache, parent=1
// 6: service=rpc1,       type=rpc,   parent=1
// 7: service=alert,      type=rpc,   parent=6
//
// Step 1: Find all time intervals to consider (set of start/end time
//         of spans):
//
//         [0, 10, 15, 20, 50, 60, 70, 80, 110, 120, 130, 150]
//
// Step 2: Map each time intervals to a set of "active" spans. A span
//         is considered active for a given time interval if it has no
//         direct child span at that time interval. This is done by
//         iterating over the spans, iterating over each time
//         intervals, and checking if the span has a child running
//         during that time interval. If not, it is considered active:
//
//         {
//             0: [ 1 ],
//             10: [ 2 ],
//             15: [ 2, 5 ],
//             20: [ 4, 5 ],
//             ...
//             110: [ 7 ],
//             120: [ 1, 7 ],
//             130: [ 7 ],
//             150: [],
//         }
//
// Step 4: Build a service and type duration mapping by:
//         1. iterating over each time intervals
//         2. computing the time interval duration portion (time
//            interval duration / number of active spans)
//         3. iterate over each active span of that time interval
//         4. add to the active span's type and service duration the
//            duration portion
//
//         {
//             web-server: 10,
//             render: 15,
//             pg: 12.5,
//             pg-read: 15,
//             redis: 27.5,
//             rpc1: 30,
//             alert: 40,
//         }
//         {
//             web: 70,
//             cache: 55,
//             db: 55,
//             rpc: 55,
//         }
func ComputeSublayers(trace pb.Trace) []SublayerValue {
	timestamps := buildTraceTimestamps(trace)
	activeSpans := buildTraceActiveSpansMapping(trace, timestamps)

	durationsByService := computeDurationByAttr(
		timestamps, activeSpans, func(s *pb.Span) string { return s.Service },
	)
	durationsByType := computeDurationByAttr(
		timestamps, activeSpans, func(s *pb.Span) string { return s.Type },
	)

	// Generate sublayers values
	values := make([]SublayerValue, 0,
		len(durationsByService)+len(durationsByType)+1,
	)

	for service, duration := range durationsByService {
		values = append(values, SublayerValue{
			Metric: "_sublayers.duration.by_service",
			Tag:    Tag{"sublayer_service", service},
			Value:  float64(int64(duration)),
		})
	}

	for spanType, duration := range durationsByType {
		values = append(values, SublayerValue{
			Metric: "_sublayers.duration.by_type",
			Tag:    Tag{"sublayer_type", spanType},
			Value:  float64(int64(duration)),
		})
	}

	values = append(values, SublayerValue{
		Metric: "_sublayers.span_count",
		Value:  float64(len(trace)),
	})

	return values
}

type Calculator struct {
	activeSpans      []int
	activeSpansIndex []int
	openedSpans      []bool
	nChildren        []int
	execPct          []float64
	parentIdx        []int
	timestamps       TimestampArray
}

func NewCalculator(n int) *Calculator {
	// todo add 1
	return &Calculator{
		activeSpans:      make([]int, n),
		activeSpansIndex: make([]int, n),
		openedSpans:      make([]bool, n),
		nChildren:        make([]int, n),
		execPct:          make([]float64, n),
		parentIdx:        make([]int, n),
		timestamps:       make(TimestampArray, 2*n),
	}
}

func (c *Calculator) reset(n int) {
	for i := 0; i < n+1; i++ {
		c.activeSpansIndex[i] = -1
		c.activeSpans[i] = 0
		c.openedSpans[i] = false
		c.nChildren[i] = 0
		c.execPct[i] = 0
	}
}

type timestamp struct {
	spanStart bool
	spanIdx    int
	parentIdx  int
	ts        int64
}

type TimestampArray []timestamp

func (t TimestampArray) Len() int      { return len(t) }
func (t TimestampArray) Swap(i, j int) { t[i], t[j] = t[j], t[i] }

// for spans with a duration of 0, we need to open them before closing them
func (t TimestampArray) Less(i, j int) bool {
	return t[i].ts < t[j].ts || (t[i].ts == t[j].ts && t[i].spanStart && !t[j].spanStart)
}

func (c *Calculator) buildNewTimestamps(trace pb.Trace) {
	for i, span := range trace {
		c.timestamps[2*i] = timestamp{spanStart: true, spanIdx: i, parentIdx: c.parentIdx[i], ts: span.Start}
		c.timestamps[2*i+1] = timestamp{spanStart: false, spanIdx: i, parentIdx: c.parentIdx[i], ts: span.Start+span.Duration}
	}
	sort.Sort(c.timestamps[:2*len(trace)])
}

func (c *Calculator) computeParentIdx(trace pb.Trace) {
	idToIdx := make(map[uint64]int, len(trace))
	for idx, span := range trace {
		idToIdx[span.SpanID] = idx
	}
	for idx, span := range trace {
		if parentIdx, ok := idToIdx[span.ParentID]; ok {
			c.parentIdx[idx] = parentIdx
		} else {
			// unknown parent (or root)
			c.parentIdx[idx] = len(trace)
		}
	}
}

func (c *Calculator) computeDurationByAttrNew(trace pb.Trace, selector attrSelector) map[string]float64 {
	durations := make(map[string]float64)
	for i, span := range trace {
		key := selector(span)
		if key == "" {
			continue
		}
		durations[key] += c.execPct[i]
	}
	return durations
}

func (c *Calculator) ComputeSublayers(trace pb.Trace) []SublayerValue {
	c.reset(len(trace))
	c.computeParentIdx(trace)
	c.buildNewTimestamps(trace)
	nActiveSpans := 0
	activate := func(spanIdx int) {
		if c.activeSpansIndex[spanIdx] == -1 {
			c.activeSpansIndex[spanIdx] = nActiveSpans
			c.activeSpans[nActiveSpans] = spanIdx
			nActiveSpans++
		}
	}

	deactivate := func(spanIdx int) {
		i := c.activeSpansIndex[spanIdx]
		if i != -1 {
			c.activeSpansIndex[c.activeSpans[nActiveSpans-1]] = i
			c.activeSpans[i] = c.activeSpans[nActiveSpans-1]
			nActiveSpans--
			c.activeSpansIndex[spanIdx] = -1
		}
	}

	var previousTs int64

	for j := 0; j < len(trace)*2; j++ {
		tp := c.timestamps[j]
		var timeIncr float64
		if nActiveSpans > 0 {
			timeIncr = float64(tp.ts - previousTs)
		}
		previousTs = tp.ts
		if timeIncr > 0 {
			timeIncr /= float64(nActiveSpans)
			for i := 0; i < nActiveSpans; i++ {
				c.execPct[c.activeSpans[i]] += timeIncr
			}
		}
		if tp.spanStart {
			deactivate(tp.parentIdx)
			c.openedSpans[tp.spanIdx] = true
			if c.nChildren[tp.spanIdx] == 0 {
				activate(tp.spanIdx)
			}
			c.nChildren[tp.parentIdx]++
		} else {
			c.openedSpans[tp.spanIdx] = false
			deactivate(tp.spanIdx)
			c.nChildren[tp.parentIdx]--
			if c.openedSpans[tp.parentIdx] && c.nChildren[tp.parentIdx] == 0 {
				activate(tp.parentIdx)
			}
		}
	}
	durationsByService := c.computeDurationByAttrNew(
		trace, func(s *pb.Span) string { return s.Service },
	)
	durationsByType := c.computeDurationByAttrNew(
		trace, func(s *pb.Span) string { return s.Type },
	)

	// Generate sublayers values
	values := make([]SublayerValue, 0,
		len(durationsByService)+len(durationsByType)+1,
	)

	for service, duration := range durationsByService {
		values = append(values, SublayerValue{
			Metric: "_sublayers.duration.by_service",
			Tag:    Tag{"sublayer_service", service},
			Value:  float64(int64(duration)),
		})
	}

	for spanType, duration := range durationsByType {
		values = append(values, SublayerValue{
			Metric: "_sublayers.duration.by_type",
			Tag:    Tag{"sublayer_type", spanType},
			Value:  float64(int64(duration)),
		})
	}

	values = append(values, SublayerValue{
		Metric: "_sublayers.span_count",
		Value:  float64(len(trace)),
	})

	return values
}

// int64Slice is used by buildTraceTimestamps as a sortable slice of
// int64
type int64Slice []int64

func (a int64Slice) Len() int           { return len(a) }
func (a int64Slice) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a int64Slice) Less(i, j int) bool { return a[i] < a[j] }

// buildTraceTimestamps returns the timestamps of a trace, i.e the set
// of start/end times of each spans
func buildTraceTimestamps(trace pb.Trace) []int64 {
	tsSet := make(map[int64]struct{}, 2*len(trace))

	for _, span := range trace {
		start, end := span.Start, span.Start+span.Duration
		tsSet[start] = struct{}{}
		tsSet[end] = struct{}{}
	}

	timestamps := make(int64Slice, 0, len(tsSet))
	for ts := range tsSet {
		timestamps = append(timestamps, ts)
	}

	sort.Sort(timestamps)
	return timestamps
}

// activeSpansMap is used by buildTraceActiveSpansMapping and is just
// a map with a add function setting the key to the empty slice of no
// entry exists
type activeSpansMap map[int64][]*pb.Span

func (a activeSpansMap) Add(ts int64, span *pb.Span) {
	if _, ok := a[ts]; !ok {
		a[ts] = make([]*pb.Span, 0, 1)
	}
	a[ts] = append(a[ts], span)
}

// buildTraceActiveSpansMapping returns a mapping from timestamps to
// a set of active spans
func buildTraceActiveSpansMapping(trace pb.Trace, timestamps []int64) map[int64][]*pb.Span {
	activeSpans := make(activeSpansMap, len(timestamps))

	tsToIdx := make(map[int64]int, len(timestamps))
	for i, ts := range timestamps {
		tsToIdx[ts] = i
	}

	spanChildren := traceutil.ChildrenMap(trace)
	for sIdx, span := range trace {
		start, end := span.Start, span.Start+span.Duration
		for tsIdx := tsToIdx[start]; tsIdx < tsToIdx[end]; tsIdx++ {
			ts := timestamps[tsIdx]

			// Do we have one of our child also in the
			// current time interval?
			hasChild := false
			for _, child := range spanChildren[span.SpanID] {
				start, end := child.Start, child.Start+child.Duration
				if start <= ts && end > ts {
					hasChild = true
					break
				}
			}

			if !hasChild {
				activeSpans.Add(ts, trace[sIdx])
			}
		}
	}

	return activeSpans
}

// attrSelector is used by computeDurationByAttr and is a func
// returning an attribute for a given span
type attrSelector func(*pb.Span) string

// computeDurationByAttr returns a mapping from an attribute to the
// sum of all weighted duration of spans with that given
// attribute. The attribute is returned by calling selector on each
// spans
func computeDurationByAttr(timestamps []int64, activeSpansByTs activeSpansMap, selector attrSelector) map[string]float64 {
	durations := make(map[string]float64)

	for i := 0; i < len(timestamps)-1; i++ {
		start := timestamps[i]
		end := timestamps[i+1]

		activeSpans := activeSpansByTs[start]
		if len(activeSpans) == 0 {
			continue
		}

		durationPortion := float64(end-start) / float64(len(activeSpans))

		for _, span := range activeSpans {
			key := selector(span)
			if key == "" {
				continue
			}

			if _, ok := durations[key]; !ok {
				durations[key] = 0
			}
			durations[key] += durationPortion
		}
	}

	return durations
}

// SetSublayersOnSpan takes some sublayers and pins them on the given span.Metrics
func SetSublayersOnSpan(span *pb.Span, values []SublayerValue) {
	if span.Metrics == nil {
		span.Metrics = make(map[string]float64, len(values))
	}

	for _, value := range values {
		name := value.Metric

		if value.Tag.Name != "" {
			name = name + "." + value.Tag.Name + ":" + value.Tag.Value
		}

		span.Metrics[name] = value.Value
	}
}
