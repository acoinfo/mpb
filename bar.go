package mpb

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/acarl005/stripansi"
	"github.com/mattn/go-runewidth"
	"github.com/vbauerster/mpb/v8/decor"
)

// Bar represents a progress bar.
type Bar struct {
	index        int // used by heap
	priority     int // used by heap
	frameCh      chan *renderFrame
	operateState chan func(*bState)
	container    *Progress
	bs           *bState
	bsOk         chan struct{}
	ctx          context.Context
	cancel       func()
}

type syncTable [2][]chan int
type extenderFunc func([]io.Reader, decor.Statistics) ([]io.Reader, error)

// bState is actual bar's state.
type bState struct {
	id                int
	priority          int
	reqWidth          int
	shutdown          int
	total             int64
	current           int64
	refill            int64
	trimSpace         bool
	completed         bool
	aborted           bool
	triggerComplete   bool
	rmOnComplete      bool
	noPop             bool
	autoRefresh       bool
	buffers           [3]*bytes.Buffer
	decorators        [2][]decor.Decorator
	averageDecorators []decor.AverageDecorator
	ewmaDecorators    []decor.EwmaDecorator
	shutdownListeners []decor.ShutdownListener
	filler            BarFiller
	extender          extenderFunc
	renderReq         chan<- time.Time
	waitBar           *Bar // key for (*pState).queueBars
}

type renderFrame struct {
	rows         []io.Reader
	shutdown     int
	rmOnComplete bool
	noPop        bool
	err          error
}

func newBar(ctx context.Context, container *Progress, bs *bState) *Bar {
	ctx, cancel := context.WithCancel(ctx)

	bar := &Bar{
		priority:     bs.priority,
		frameCh:      make(chan *renderFrame, 1),
		operateState: make(chan func(*bState)),
		bsOk:         make(chan struct{}),
		container:    container,
		ctx:          ctx,
		cancel:       cancel,
	}

	container.bwg.Add(1)
	go bar.serve(bs)
	return bar
}

// ProxyReader wraps io.Reader with metrics required for progress
// tracking. If `r` is 'unknown total/size' reader it's mandatory
// to call `(*Bar).SetTotal(-1, true)` after the wrapper returns
// `io.EOF`. If bar is already completed or aborted, returns nil.
// Panics if `r` is nil.
func (b *Bar) ProxyReader(r io.Reader) io.ReadCloser {
	if r == nil {
		panic("expected non nil io.Reader")
	}
	result := make(chan io.ReadCloser)
	select {
	case b.operateState <- func(s *bState) {
		result <- newProxyReader(r, b, len(s.ewmaDecorators) != 0)
	}:
		return <-result
	case <-b.ctx.Done():
		return nil
	}
}

// ProxyWriter wraps io.Writer with metrics required for progress tracking.
// If bar is already completed or aborted, returns nil.
// Panics if `w` is nil.
func (b *Bar) ProxyWriter(w io.Writer) io.WriteCloser {
	if w == nil {
		panic("expected non nil io.Writer")
	}
	result := make(chan io.WriteCloser)
	select {
	case b.operateState <- func(s *bState) {
		result <- newProxyWriter(w, b, len(s.ewmaDecorators) != 0)
	}:
		return <-result
	case <-b.ctx.Done():
		return nil
	}
}

// ID returs id of the bar.
func (b *Bar) ID() int {
	result := make(chan int)
	select {
	case b.operateState <- func(s *bState) { result <- s.id }:
		return <-result
	case <-b.bsOk:
		return b.bs.id
	}
}

// Current returns bar's current value, in other words sum of all increments.
func (b *Bar) Current() int64 {
	result := make(chan int64)
	select {
	case b.operateState <- func(s *bState) { result <- s.current }:
		return <-result
	case <-b.bsOk:
		return b.bs.current
	}
}

// SetRefill sets refill flag with specified amount.
// The underlying BarFiller will change its visual representation, to
// indicate refill event. Refill event may be referred to some retry
// operation for example.
func (b *Bar) SetRefill(amount int64) {
	select {
	case b.operateState <- func(s *bState) {
		if amount < s.current {
			s.refill = amount
		} else {
			s.refill = s.current
		}
	}:
	case <-b.ctx.Done():
	}
}

// TraverseDecorators traverses all available decorators and calls cb func on each.
func (b *Bar) TraverseDecorators(cb func(decor.Decorator)) {
	iter := make(chan decor.Decorator)
	select {
	case b.operateState <- func(s *bState) {
		for _, decorators := range s.decorators {
			for _, d := range decorators {
				iter <- d
			}
		}
		close(iter)
	}:
		for d := range iter {
			cb(unwrap(d))
		}
	case <-b.ctx.Done():
	}
}

// EnableTriggerComplete enables triggering complete event. It's effective
// only for bars which were constructed with `total <= 0`. If `curren >= total`
// at the moment of call, complete event is triggered right away.
func (b *Bar) EnableTriggerComplete() {
	select {
	case b.operateState <- func(s *bState) {
		if s.triggerComplete {
			return
		}
		if s.current >= s.total {
			s.current = s.total
			s.completed = true
			s.triggerCompletion(b)
		} else {
			s.triggerComplete = true
		}
	}:
	case <-b.ctx.Done():
	}
}

// SetTotal sets total to an arbitrary value. It's effective only for bar
// which was constructed with `total <= 0`. Setting total to negative value
// is equivalent to `(*Bar).SetTotal((*Bar).Current(), bool)` but faster.
// If `complete` is true complete event is triggered right away.
// Calling `(*Bar).EnableTriggerComplete` makes this one no operational.
func (b *Bar) SetTotal(total int64, complete bool) {
	select {
	case b.operateState <- func(s *bState) {
		if s.triggerComplete {
			return
		}
		if total < 0 {
			s.total = s.current
		} else {
			s.total = total
		}
		if complete {
			s.current = s.total
			s.completed = true
			s.triggerCompletion(b)
		}
	}:
	case <-b.ctx.Done():
	}
}

// SetCurrent sets progress' current to an arbitrary value.
func (b *Bar) SetCurrent(current int64) {
	if current < 0 {
		return
	}
	select {
	case b.operateState <- func(s *bState) {
		s.current = current
		if s.triggerComplete && s.current >= s.total {
			s.current = s.total
			s.completed = true
			s.triggerCompletion(b)
		}
	}:
	case <-b.ctx.Done():
	}
}

// EwmaSetCurrent sets progress' current to an arbitrary value and updates
// EWMA based decorators by dur of a single iteration.
func (b *Bar) EwmaSetCurrent(current int64, iterDur time.Duration) {
	if current < 0 {
		return
	}
	result := make(chan *sync.WaitGroup)
	select {
	case b.operateState <- func(s *bState) {
		var wg sync.WaitGroup
		s.decoratorEwmaUpdate(current-s.current, iterDur, &wg)
		s.current = current
		if s.triggerComplete && s.current >= s.total {
			s.current = s.total
			s.completed = true
			s.triggerCompletion(b)
		}
		result <- &wg
	}:
		wg := <-result
		wg.Wait()
	case <-b.ctx.Done():
	}
}

// Increment is a shorthand for b.IncrInt64(1).
func (b *Bar) Increment() {
	b.IncrInt64(1)
}

// IncrBy is a shorthand for b.IncrInt64(int64(n)).
func (b *Bar) IncrBy(n int) {
	b.IncrInt64(int64(n))
}

// IncrInt64 increments progress by amount of n.
func (b *Bar) IncrInt64(n int64) {
	select {
	case b.operateState <- func(s *bState) {
		s.current += n
		if s.triggerComplete && s.current >= s.total {
			s.current = s.total
			s.completed = true
			s.triggerCompletion(b)
		}
	}:
	case <-b.ctx.Done():
	}
}

// EwmaIncrement is a shorthand for b.EwmaIncrInt64(1, iterDur).
func (b *Bar) EwmaIncrement(iterDur time.Duration) {
	b.EwmaIncrInt64(1, iterDur)
}

// EwmaIncrBy is a shorthand for b.EwmaIncrInt64(int64(n), iterDur).
func (b *Bar) EwmaIncrBy(n int, iterDur time.Duration) {
	b.EwmaIncrInt64(int64(n), iterDur)
}

// EwmaIncrInt64 increments progress by amount of n and updates EWMA based
// decorators by dur of a single iteration.
func (b *Bar) EwmaIncrInt64(n int64, iterDur time.Duration) {
	result := make(chan *sync.WaitGroup)
	select {
	case b.operateState <- func(s *bState) {
		var wg sync.WaitGroup
		s.decoratorEwmaUpdate(n, iterDur, &wg)
		s.current += n
		if s.triggerComplete && s.current >= s.total {
			s.current = s.total
			s.completed = true
			s.triggerCompletion(b)
		}
		result <- &wg
	}:
		wg := <-result
		wg.Wait()
	case <-b.ctx.Done():
	}
}

// DecoratorAverageAdjust adjusts all average based decorators. Call
// if you need to adjust start time of all average based decorators
// or after progress resume.
func (b *Bar) DecoratorAverageAdjust(start time.Time) {
	result := make(chan *sync.WaitGroup)
	select {
	case b.operateState <- func(s *bState) {
		var wg sync.WaitGroup
		s.decoratorAverageAdjust(start, &wg)
		result <- &wg
	}:
		wg := <-result
		wg.Wait()
	case <-b.ctx.Done():
	}
}

// SetPriority changes bar's order among multiple bars. Zero is highest
// priority, i.e. bar will be on top. If you don't need to set priority
// dynamically, better use BarPriority option.
func (b *Bar) SetPriority(priority int) {
	b.container.UpdateBarPriority(b, priority, false)
}

// Abort interrupts bar's running goroutine. Abort won't be engaged
// if bar is already in complete state. If drop is true bar will be
// removed as well. To make sure that bar has been removed call
// `(*Bar).Wait()` method.
func (b *Bar) Abort(drop bool) {
	select {
	case b.operateState <- func(s *bState) {
		if s.completed || s.aborted {
			return
		}
		s.aborted = true
		s.rmOnComplete = drop
		s.triggerCompletion(b)
	}:
	case <-b.ctx.Done():
	}
}

// Aborted reports whether the bar is in aborted state.
func (b *Bar) Aborted() bool {
	result := make(chan bool)
	select {
	case b.operateState <- func(s *bState) { result <- s.aborted }:
		return <-result
	case <-b.bsOk:
		return b.bs.aborted
	}
}

// Completed reports whether the bar is in completed state.
func (b *Bar) Completed() bool {
	result := make(chan bool)
	select {
	case b.operateState <- func(s *bState) { result <- s.completed }:
		return <-result
	case <-b.bsOk:
		return b.bs.completed
	}
}

// IsRunning reports whether the bar is in running state.
func (b *Bar) IsRunning() bool {
	select {
	case <-b.ctx.Done():
		return false
	default:
		return true
	}
}

// Wait blocks until bar is completed or aborted.
func (b *Bar) Wait() {
	<-b.bsOk
}

func (b *Bar) serve(bs *bState) {
	for {
		select {
		case op := <-b.operateState:
			op(bs)
		case <-b.ctx.Done():
			bs.aborted = !bs.completed
			bs.decoratorShutdownNotify(&b.container.bwg)
			b.bs = bs
			close(b.bsOk)
			b.container.bwg.Done()
			return
		}
	}
}

func (b *Bar) render(tw int) {
	fn := func(s *bState) {
		frame := new(renderFrame)
		stat := newStatistics(tw, s)
		r, err := s.draw(stat)
		if err != nil {
			for _, buf := range s.buffers {
				buf.Reset()
			}
			frame.err = err
			b.frameCh <- frame
			return
		}
		frame.rows = append(frame.rows, r)
		if s.extender != nil {
			frame.rows, frame.err = s.extender(frame.rows, stat)
		}
		if s.completed || s.aborted {
			frame.shutdown = s.shutdown
			frame.rmOnComplete = s.rmOnComplete
			frame.noPop = s.noPop
			// post increment makes sure OnComplete decorators are rendered
			s.shutdown++
		}
		b.frameCh <- frame
	}
	select {
	case b.operateState <- fn:
	case <-b.bsOk:
		fn(b.bs)
	}
}

func (b *Bar) tryEarlyRefresh(renderReq chan<- time.Time) {
	var otherRunning int
	b.container.traverseBars(func(bar *Bar) bool {
		if b != bar && bar.IsRunning() {
			otherRunning++
			return false // stop traverse
		}
		return true // continue traverse
	})
	if otherRunning == 0 {
		for {
			select {
			case renderReq <- time.Now():
			case <-b.ctx.Done():
				return
			}
		}
	}
}

func (b *Bar) wSyncTable() syncTable {
	result := make(chan syncTable)
	select {
	case b.operateState <- func(s *bState) { result <- s.wSyncTable() }:
		return <-result
	case <-b.bsOk:
		return b.bs.wSyncTable()
	}
}

func (s *bState) draw(stat decor.Statistics) (_ io.Reader, err error) {
	decorFiller := func(buf *bytes.Buffer, decorators []decor.Decorator) (err error) {
		for _, d := range decorators {
			// need to call Decor in any case becase of width synchronization
			str, width := d.Decor(stat)
			if err != nil {
				continue
			}
			if w := stat.AvailableWidth - width; w >= 0 {
				_, err = buf.WriteString(str)
				stat.AvailableWidth = w
			} else if stat.AvailableWidth > 0 {
				trunc := runewidth.Truncate(stripansi.Strip(str), stat.AvailableWidth, "…")
				_, err = buf.WriteString(trunc)
				stat.AvailableWidth = 0
			}
		}
		return err
	}

	for i, buf := range s.buffers[:2] {
		err = decorFiller(buf, s.decorators[i])
		if err != nil {
			return nil, err
		}
	}

	spaces := []io.Reader{
		strings.NewReader(" "),
		strings.NewReader(" "),
	}
	if s.trimSpace || stat.AvailableWidth < 2 {
		for _, r := range spaces {
			_, _ = io.Copy(io.Discard, r)
		}
	} else {
		stat.AvailableWidth -= 2
	}

	err = s.filler.Fill(s.buffers[2], stat)
	if err != nil {
		return nil, err
	}

	return io.MultiReader(
		s.buffers[0],
		spaces[0],
		s.buffers[2],
		spaces[1],
		s.buffers[1],
		strings.NewReader("\n"),
	), nil
}

func (s *bState) wSyncTable() (table syncTable) {
	var count int
	var row []chan int

	for i, decorators := range s.decorators {
		for _, d := range decorators {
			if ch, ok := d.Sync(); ok {
				row = append(row, ch)
				count++
			}
		}
		switch i {
		case 0:
			table[i] = row[0:count]
		default:
			table[i] = row[len(table[i-1]):count]
		}
	}
	return table
}

func (s bState) triggerCompletion(b *Bar) {
	if s.autoRefresh {
		// Technically this call isn't required, but if refresh rate is set to
		// one hour for example and bar completes within a few minutes p.Wait()
		// will wait for one hour. This call helps to avoid unnecessary waiting.
		go b.tryEarlyRefresh(s.renderReq)
	} else {
		b.cancel()
	}
}

func (s bState) decoratorEwmaUpdate(n int64, dur time.Duration, wg *sync.WaitGroup) {
	for i := 0; i < len(s.ewmaDecorators); i++ {
		wg.Add(1)
		switch d := s.ewmaDecorators[i]; i {
		case len(s.ewmaDecorators) - 1:
			d.EwmaUpdate(n, dur)
			wg.Done()
		default:
			go func() {
				d.EwmaUpdate(n, dur)
				wg.Done()
			}()
		}
	}
}

func (s bState) decoratorAverageAdjust(start time.Time, wg *sync.WaitGroup) {
	for i := 0; i < len(s.averageDecorators); i++ {
		wg.Add(1)
		switch d := s.averageDecorators[i]; i {
		case len(s.averageDecorators) - 1:
			d.AverageAdjust(start)
			wg.Done()
		default:
			go func() {
				d.AverageAdjust(start)
				wg.Done()
			}()
		}
	}
}

func (s bState) decoratorShutdownNotify(wg *sync.WaitGroup) {
	for i := 0; i < len(s.shutdownListeners); i++ {
		wg.Add(1)
		switch d := s.shutdownListeners[i]; i {
		case len(s.shutdownListeners) - 1:
			d.OnShutdown()
			wg.Done()
		default:
			go func() {
				d.OnShutdown()
				wg.Done()
			}()
		}
	}
}

func newStatistics(tw int, s *bState) decor.Statistics {
	return decor.Statistics{
		AvailableWidth: tw,
		RequestedWidth: s.reqWidth,
		ID:             s.id,
		Total:          s.total,
		Current:        s.current,
		Refill:         s.refill,
		Completed:      s.completed,
		Aborted:        s.aborted,
	}
}

func unwrap(d decor.Decorator) decor.Decorator {
	if d, ok := d.(decor.Wrapper); ok {
		return unwrap(d.Unwrap())
	}
	return d
}
