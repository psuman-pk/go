// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Serving of pprof-like profiles.

package main

import (
	"bufio"
	"fmt"
	"internal/trace"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/google/pprof/profile"
)

func goCmd() string {
	var exeSuffix string
	if runtime.GOOS == "windows" {
		exeSuffix = ".exe"
	}
	path := filepath.Join(runtime.GOROOT(), "bin", "go"+exeSuffix)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return "go"
}

func init() {
	http.HandleFunc("/io", serveSVGProfile(pprofByGoroutine(computePprofIO)))
	http.HandleFunc("/block", serveSVGProfile(pprofByGoroutine(computePprofBlock)))
	http.HandleFunc("/syscall", serveSVGProfile(pprofByGoroutine(computePprofSyscall)))
	http.HandleFunc("/sched", serveSVGProfile(pprofByGoroutine(computePprofSched)))

	http.HandleFunc("/spanio", serveSVGProfile(pprofBySpan(computePprofIO)))
	http.HandleFunc("/spanblock", serveSVGProfile(pprofBySpan(computePprofBlock)))
	http.HandleFunc("/spansyscall", serveSVGProfile(pprofBySpan(computePprofSyscall)))
	http.HandleFunc("/spansched", serveSVGProfile(pprofBySpan(computePprofSched)))
}

// Record represents one entry in pprof-like profiles.
type Record struct {
	stk  []*trace.Frame
	n    uint64
	time int64
}

// interval represents a time interval in the trace.
type interval struct {
	begin, end int64 // nanoseconds.
}

func pprofByGoroutine(compute func(io.Writer, map[uint64][]interval, []*trace.Event) error) func(w io.Writer, r *http.Request) error {
	return func(w io.Writer, r *http.Request) error {
		id := r.FormValue("id")
		events, err := parseEvents()
		if err != nil {
			return err
		}
		gToIntervals, err := pprofMatchingGoroutines(id, events)
		if err != nil {
			return err
		}
		return compute(w, gToIntervals, events)
	}
}

func pprofBySpan(compute func(io.Writer, map[uint64][]interval, []*trace.Event) error) func(w io.Writer, r *http.Request) error {
	return func(w io.Writer, r *http.Request) error {
		filter, err := newSpanFilter(r)
		if err != nil {
			return err
		}
		gToIntervals, err := pprofMatchingSpans(filter)
		if err != nil {
			return err
		}
		events, _ := parseEvents()

		return compute(w, gToIntervals, events)
	}
}

// pprofMatchingGoroutines parses the goroutine type id string (i.e. pc)
// and returns the ids of goroutines of the matching type and its interval.
// If the id string is empty, returns nil without an error.
func pprofMatchingGoroutines(id string, events []*trace.Event) (map[uint64][]interval, error) {
	if id == "" {
		return nil, nil
	}
	pc, err := strconv.ParseUint(id, 10, 64) // id is string
	if err != nil {
		return nil, fmt.Errorf("invalid goroutine type: %v", id)
	}
	analyzeGoroutines(events)
	var res map[uint64][]interval
	for _, g := range gs {
		if g.PC != pc {
			continue
		}
		if res == nil {
			res = make(map[uint64][]interval)
		}
		endTime := g.EndTime
		if g.EndTime == 0 {
			endTime = lastTimestamp() // the trace doesn't include the goroutine end event. Use the trace end time.
		}
		res[g.ID] = []interval{{begin: g.StartTime, end: endTime}}
	}
	if len(res) == 0 && id != "" {
		return nil, fmt.Errorf("failed to find matching goroutines for id: %s", id)
	}
	return res, nil
}

// pprofMatchingSpans returns the time intervals of matching spans
// grouped by the goroutine id. If the filter is nil, returns nil without an error.
func pprofMatchingSpans(filter *spanFilter) (map[uint64][]interval, error) {
	res, err := analyzeAnnotations()
	if err != nil {
		return nil, err
	}
	if filter == nil {
		return nil, nil
	}

	gToIntervals := make(map[uint64][]interval)
	for id, spans := range res.spans {
		for _, s := range spans {
			if filter.match(id, s) {
				gToIntervals[s.G] = append(gToIntervals[s.G], interval{begin: s.firstTimestamp(), end: s.lastTimestamp()})
			}
		}
	}

	for g, intervals := range gToIntervals {
		// in order to remove nested spans and
		// consider only the outermost spans,
		// first, we sort based on the start time
		// and then scan through to select only the outermost spans.
		sort.Slice(intervals, func(i, j int) bool {
			x := intervals[i].begin
			y := intervals[j].begin
			if x == y {
				return intervals[i].end < intervals[j].end
			}
			return x < y
		})
		var lastTimestamp int64
		var n int
		// select only the outermost spans.
		for _, i := range intervals {
			if lastTimestamp <= i.begin {
				intervals[n] = i // new non-overlapping span starts.
				lastTimestamp = i.end
				n++
			} // otherwise, skip because this span overlaps with a previous span.
		}
		gToIntervals[g] = intervals[:n]
	}
	return gToIntervals, nil
}

// computePprofIO generates IO pprof-like profile (time spent in IO wait, currently only network blocking event).
func computePprofIO(w io.Writer, gToIntervals map[uint64][]interval, events []*trace.Event) error {
	prof := make(map[uint64]Record)
	for _, ev := range events {
		if ev.Type != trace.EvGoBlockNet || ev.Link == nil || ev.StkID == 0 || len(ev.Stk) == 0 {
			continue
		}
		overlapping := pprofOverlappingDuration(gToIntervals, ev)
		if overlapping > 0 {
			rec := prof[ev.StkID]
			rec.stk = ev.Stk
			rec.n++
			rec.time += overlapping.Nanoseconds()
			prof[ev.StkID] = rec
		}
	}
	return buildProfile(prof).Write(w)
}

// computePprofBlock generates blocking pprof-like profile (time spent blocked on synchronization primitives).
func computePprofBlock(w io.Writer, gToIntervals map[uint64][]interval, events []*trace.Event) error {
	prof := make(map[uint64]Record)
	for _, ev := range events {
		switch ev.Type {
		case trace.EvGoBlockSend, trace.EvGoBlockRecv, trace.EvGoBlockSelect,
			trace.EvGoBlockSync, trace.EvGoBlockCond, trace.EvGoBlockGC:
			// TODO(hyangah): figure out why EvGoBlockGC should be here.
			// EvGoBlockGC indicates the goroutine blocks on GC assist, not
			// on synchronization primitives.
		default:
			continue
		}
		if ev.Link == nil || ev.StkID == 0 || len(ev.Stk) == 0 {
			continue
		}
		overlapping := pprofOverlappingDuration(gToIntervals, ev)
		if overlapping > 0 {
			rec := prof[ev.StkID]
			rec.stk = ev.Stk
			rec.n++
			rec.time += overlapping.Nanoseconds()
			prof[ev.StkID] = rec
		}
	}
	return buildProfile(prof).Write(w)
}

// computePprofSyscall generates syscall pprof-like profile (time spent blocked in syscalls).
func computePprofSyscall(w io.Writer, gToIntervals map[uint64][]interval, events []*trace.Event) error {
	prof := make(map[uint64]Record)
	for _, ev := range events {
		if ev.Type != trace.EvGoSysCall || ev.Link == nil || ev.StkID == 0 || len(ev.Stk) == 0 {
			continue
		}
		overlapping := pprofOverlappingDuration(gToIntervals, ev)
		if overlapping > 0 {
			rec := prof[ev.StkID]
			rec.stk = ev.Stk
			rec.n++
			rec.time += overlapping.Nanoseconds()
			prof[ev.StkID] = rec
		}
	}
	return buildProfile(prof).Write(w)
}

// computePprofSched generates scheduler latency pprof-like profile
// (time between a goroutine become runnable and actually scheduled for execution).
func computePprofSched(w io.Writer, gToIntervals map[uint64][]interval, events []*trace.Event) error {
	prof := make(map[uint64]Record)
	for _, ev := range events {
		if (ev.Type != trace.EvGoUnblock && ev.Type != trace.EvGoCreate) ||
			ev.Link == nil || ev.StkID == 0 || len(ev.Stk) == 0 {
			continue
		}
		overlapping := pprofOverlappingDuration(gToIntervals, ev)
		if overlapping > 0 {
			rec := prof[ev.StkID]
			rec.stk = ev.Stk
			rec.n++
			rec.time += overlapping.Nanoseconds()
			prof[ev.StkID] = rec
		}
	}
	return buildProfile(prof).Write(w)
}

// pprofOverlappingDuration returns the overlapping duration between
// the time intervals in gToIntervals and the specified event.
// If gToIntervals is nil, this simply returns the event's duration.
func pprofOverlappingDuration(gToIntervals map[uint64][]interval, ev *trace.Event) time.Duration {
	if gToIntervals == nil { // No filtering.
		return time.Duration(ev.Link.Ts-ev.Ts) * time.Nanosecond
	}
	intervals := gToIntervals[ev.G]
	if len(intervals) == 0 {
		return 0
	}

	var overlapping time.Duration
	for _, i := range intervals {
		if o := overlappingDuration(i.begin, i.end, ev.Ts, ev.Link.Ts); o > 0 {
			overlapping += o
		}
	}
	return overlapping
}

// serveSVGProfile serves pprof-like profile generated by prof as svg.
func serveSVGProfile(prof func(w io.Writer, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		if r.FormValue("raw") != "" {
			w.Header().Set("Content-Type", "application/octet-stream")
			if err := prof(w, r); err != nil {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("X-Go-Pprof", "1")
				http.Error(w, fmt.Sprintf("failed to get profile: %v", err), http.StatusInternalServerError)
				return
			}
			return
		}

		blockf, err := ioutil.TempFile("", "block")
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to create temp file: %v", err), http.StatusInternalServerError)
			return
		}
		defer func() {
			blockf.Close()
			os.Remove(blockf.Name())
		}()
		blockb := bufio.NewWriter(blockf)
		if err := prof(blockb, r); err != nil {
			http.Error(w, fmt.Sprintf("failed to generate profile: %v", err), http.StatusInternalServerError)
			return
		}
		if err := blockb.Flush(); err != nil {
			http.Error(w, fmt.Sprintf("failed to flush temp file: %v", err), http.StatusInternalServerError)
			return
		}
		if err := blockf.Close(); err != nil {
			http.Error(w, fmt.Sprintf("failed to close temp file: %v", err), http.StatusInternalServerError)
			return
		}
		svgFilename := blockf.Name() + ".svg"
		if output, err := exec.Command(goCmd(), "tool", "pprof", "-svg", "-output", svgFilename, blockf.Name()).CombinedOutput(); err != nil {
			http.Error(w, fmt.Sprintf("failed to execute go tool pprof: %v\n%s", err, output), http.StatusInternalServerError)
			return
		}
		defer os.Remove(svgFilename)
		w.Header().Set("Content-Type", "image/svg+xml")
		http.ServeFile(w, r, svgFilename)
	}
}

func buildProfile(prof map[uint64]Record) *profile.Profile {
	p := &profile.Profile{
		PeriodType: &profile.ValueType{Type: "trace", Unit: "count"},
		Period:     1,
		SampleType: []*profile.ValueType{
			{Type: "contentions", Unit: "count"},
			{Type: "delay", Unit: "nanoseconds"},
		},
	}
	locs := make(map[uint64]*profile.Location)
	funcs := make(map[string]*profile.Function)
	for _, rec := range prof {
		var sloc []*profile.Location
		for _, frame := range rec.stk {
			loc := locs[frame.PC]
			if loc == nil {
				fn := funcs[frame.File+frame.Fn]
				if fn == nil {
					fn = &profile.Function{
						ID:         uint64(len(p.Function) + 1),
						Name:       frame.Fn,
						SystemName: frame.Fn,
						Filename:   frame.File,
					}
					p.Function = append(p.Function, fn)
					funcs[frame.File+frame.Fn] = fn
				}
				loc = &profile.Location{
					ID:      uint64(len(p.Location) + 1),
					Address: frame.PC,
					Line: []profile.Line{
						profile.Line{
							Function: fn,
							Line:     int64(frame.Line),
						},
					},
				}
				p.Location = append(p.Location, loc)
				locs[frame.PC] = loc
			}
			sloc = append(sloc, loc)
		}
		p.Sample = append(p.Sample, &profile.Sample{
			Value:    []int64{int64(rec.n), rec.time},
			Location: sloc,
		})
	}
	return p
}
