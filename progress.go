package main

import (
	"io"
	"sync"
	"time"
)

// progress is what a build is doing right now, shown on /health.
var progress = &buildProgress{}

type buildProgress struct {
	mu        sync.Mutex
	running   bool
	dump      string
	step      string // "downloading mbdump.tar.bz2", "loading track", "building indexes and search"
	done      int64  // bytes of the current file downloaded or read
	total     int64
	started   time.Time
	lastError string
	nextCheck time.Time
}

func (p *buildProgress) setNext(t time.Time) {
	p.mu.Lock()
	p.nextCheck = t
	p.mu.Unlock()
}

func (p *buildProgress) isRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

func (p *buildProgress) set(step string, done, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.step, p.done, p.total = step, done, total
}

func (p *buildProgress) add(n int64) {
	p.mu.Lock()
	p.done += n
	p.mu.Unlock()
}

func (p *buildProgress) begin(dump string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running, p.dump, p.started = true, dump, time.Now()
	p.step, p.done, p.total = "", 0, 0
}

func (p *buildProgress) end(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running, p.step, p.done, p.total = false, "", 0, 0
	p.lastError = ""
	if err != nil {
		p.lastError = err.Error()
	}
}

func (p *buildProgress) report() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]any{"building": p.running}
	if p.running {
		out["dump"], out["step"] = p.dump, p.step
		out["started"] = p.started.UTC().Format(time.RFC3339)
		if p.total > 0 {
			out["percent"] = p.done * 100 / p.total
		}
	}
	if p.lastError != "" {
		out["last_error"] = p.lastError
	}
	if !p.nextCheck.IsZero() {
		out["next_check"] = p.nextCheck.UTC().Format(time.RFC3339)
	}
	return out
}

// counting adds what passes through a reader to the build's progress.
type counting struct{ r io.Reader }

func (c counting) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	progress.add(int64(n))
	return n, err
}
