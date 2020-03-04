package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"reflect"
	"time"
)

func setDefaultTimeout(ctx context.Context, t time.Duration) (context.Context, context.CancelFunc) {
	var cancel context.CancelFunc = func() {}
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, t)
	}
	return ctx, cancel
}

func getProcessTimeout(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Duration(0)
	}
	//empirical value 1/2
	return deadline.Sub(time.Now()) / 2
}

//Req type
type Req interface{}

//Resp type
type Resp interface{}

//RespErr response with error
type RespErr struct {
	Result Resp
	Err    error
}

type respErrIdx struct {
	RespErr
	idx int
}

//Handler prototype for request handler.
type Handler func(ctx context.Context, r Req) (Resp, error)

//Gather gather all responses, and process them.
type Gather func(ctx context.Context, resps []Resp) (Resp, error)

//prepare channel and start a goroutine do Handler.
func doHandle(ctx context.Context, r Req, h Handler) <-chan RespErr {
	c := make(chan RespErr, 1)
	//take care when pass by reference!!
	go func() {
		//close c in only sender, indicates no more data can be read, needless here.
		defer close(c)
		var resp RespErr
		resp.Result, resp.Err = h(ctx, r)
		log.Printf("[process] Req: %+v, return: %+v, error: %+v\n", r, resp.Result, resp.Err)
		c <- resp
	}()
	return c
}

//for each request, set up a channel, start a goroutine, wait for response, and send response to the channel.
func prepare(ctx context.Context, reqs []Req, handlers []Handler) ([]<-chan RespErr, context.CancelFunc) {
	cancels := make([]context.CancelFunc, len(reqs))
	cancel := func() {
		for _, cancel := range cancels {
			cancel()
		}
	}
	chans := make([]<-chan RespErr, len(reqs))
	for i, Req := range reqs {
		ReqCtx, ReqCancel := context.WithTimeout(ctx, getProcessTimeout(ctx))
		cancels[i] = ReqCancel
		chans[i] = doHandle(ReqCtx, Req, handlers[i])
	}
	return chans, cancel
}

type waitController interface {
	//early return when recv response
	earlyReturn(RespErr) bool
	//check if loop is over
	breakLoop([]*RespErr) bool
}

type waitAnyController int

const waitAnyControllerConst = waitAnyController(0)

//any Request succeeded
func (waitAnyController) earlyReturn(r RespErr) bool {
	return r.Err == nil
}

//all failed
func (waitAnyController) breakLoop(resps []*RespErr) bool {
	for _, resp := range resps {
		if resp == nil {
			return false
		}
	}
	return true
}

type waitPrioController int

const waitPrioControllerConst = waitPrioController(0)

//useless here, waitprio need global info, highest priority succeeded may early return
func (waitPrioController) earlyReturn(r RespErr) bool {
	return false
}

//wait priority succeeded or all failed
func (waitPrioController) breakLoop(resps []*RespErr) bool {
	allFailures := true
	for _, resp := range resps {
		if resp != nil {
			if resp.Err == nil {
				return true
			}
		} else {
			allFailures = false
		}
	}
	return allFailures
}

type waitAllController int

const waitAllControllerConst = waitAllController(0)

//any Request failed
func (waitAllController) earlyReturn(r RespErr) bool {
	return r.Err != nil
}

//all succeeded
func (waitAllController) breakLoop(resps []*RespErr) bool {
	for _, resp := range resps {
		if resp == nil {
			return false
		}
	}
	return true
}

//start goroutine for each request, wait for response, and send response back to single channel(arg input).
func prepareSingleChan(ctx context.Context, reqs []Req, handlers []Handler, c chan<- respErrIdx, start int) context.CancelFunc {
	cancels := make([]context.CancelFunc, len(reqs))
	cancel := func() {
		for _, cancel := range cancels {
			cancel()
		}
	}
	for i := range reqs {
		ReqCtx, ReqCancel := context.WithTimeout(ctx, getProcessTimeout(ctx))
		cancels[i] = ReqCancel
		go func(i int) {
			result, err := handlers[i](ReqCtx, reqs[i])
			c <- respErrIdx{RespErr{result, err}, i + start}
		}(i)
	}
	return cancel
}

func waitSingleChan(ctx context.Context, reqs []Req, handlers []Handler, c waitController, delay time.Duration) ([]*RespErr, error) {
	next := len(reqs)
	var delayTimer *time.Timer
	waitDelayTimeout := func() <-chan time.Time {
		if delayTimer == nil {
			return nil
		}
		return delayTimer.C
	}
	if delay != 0 {
		delayTimer = time.NewTimer(delay)
		next = 1
	}

	respErrIdxChan := make(chan respErrIdx, len(reqs))
	cancel := prepareSingleChan(ctx, reqs[:next], handlers[:next], respErrIdxChan, 0)
	defer cancel()

	RespErrs := make([]*RespErr, len(reqs))
	for {
		if c.breakLoop(RespErrs) {
			break
		}
		more := func() bool {
			return next != len(reqs)
		}
		fastTriggerNext := func() {
			//if trigger exists, not expired and there remains untriggered Handler, let it run immediately
			if delayTimer != nil && delayTimer.Stop() && more() {
				delayTimer.Reset(0)
			}
		}
		//select: listen on both callees's channel returns and caller's context cancel.
		select {
		case <-waitDelayTimeout():
			if more() {
				//run next Handler, SELECT on it, prepare next trigger if there remains
				cancel := prepareSingleChan(ctx, reqs[next:next+1], handlers[next:next+1], respErrIdxChan, next)
				defer cancel()
				next++
				if more() {
					delayTimer.Reset(delay)
				}
			}
		case respErrIdx, ok := <-respErrIdxChan:
			chosen := respErrIdx.idx
			if !ok {
				panic("never happened")
			}
			RespErrs[chosen] = &respErrIdx.RespErr
			if c.earlyReturn(respErrIdx.RespErr) {
				return RespErrs, respErrIdx.Err
			}
			//if last one failed, faster trigger next Handler
			if chosen+1 == next {
				fastTriggerNext()
			}
		case <-ctx.Done():
			return RespErrs, ctx.Err()
		}
	}
	return RespErrs, nil
}

//waitPerReqChan wait for resps one chan per request.
//use reflect select for variadic number of requests.
//for loop on select until either ctx cancle or we recieve all response.
func waitPerReqChan(ctx context.Context, reqs []Req, handlers []Handler, c waitController, delay time.Duration) ([]*RespErr, error) {
	next := len(reqs)
	var delayTimer *time.Timer
	waitDelayTimeout := func() <-chan time.Time {
		if delayTimer == nil {
			return nil
		}
		return delayTimer.C
	}
	if delay != 0 {
		delayTimer = time.NewTimer(delay)
		next = 1
	}

	chans, cancel := prepare(ctx, reqs[:next], handlers[:next])
	defer cancel()

	RespErrs := make([]*RespErr, len(reqs))
	//dynamic way, readability & performance penalty(https://golang.org/pkg/reflect/#Select)
	var cases = make([]reflect.SelectCase, next+2, len(reqs)+2)
	cases[0] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())}
	cases[1] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(waitDelayTimeout())}
	for i, c := range chans {
		cases[i+2] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(c)}
	}
	more := func() bool {
		return next != len(reqs)
	}
	fastTriggerNext := func() {
		//if trigger exists, not expired and there remains untriggered Handler, let it run immediately
		if delayTimer != nil && delayTimer.Stop() && more() {
			delayTimer.Reset(0)
		}
	}
	for {
		if c.breakLoop(RespErrs) {
			break
		}
		chosen, recv, recvOK := reflect.Select(cases)
		switch chosen {
		case 0:
			return RespErrs, ctx.Err()
		case 1:
			if more() {
				//run next Handler, SELECT on it, prepare next trigger if there remains
				chans, cancel := prepare(ctx, reqs[next:next+1], handlers[next:next+1])
				defer cancel()
				cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(chans[0])})
				next++
				if more() {
					delayTimer.Reset(delay)
				}
			}
		default:
			cases[chosen] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(nil)}
			chosen -= 2
			if !recvOK {
				panic("never happened")
			}
			RespErr, ok := recv.Interface().(RespErr)
			if !ok {
				panic("never happened")
			}
			RespErrs[chosen] = &RespErr
			if c.earlyReturn(RespErr) {
				return RespErrs, RespErr.Err
			}
			//if last one failed, faster trigger next Handler
			if chosen+1 == next {
				fastTriggerNext()
			}
		}
	}
	return RespErrs, nil
}

func getChan(chans []<-chan RespErr, i int) <-chan RespErr {
	if i < len(chans) {
		return chans[i]
	}
	return nil
}

//waitPerReqChanConst wait for resps one chan per request with const number of request(len(reqs) <= 4)
//set select case recv chan as nil after receive from it as we want only one response for one request.
//for loop on select until either ctx cancle or we recieve all response.
func waitPerReqChanConst(ctx context.Context, reqs []Req, handlers []Handler, c waitController, delay time.Duration) ([]*RespErr, error) {
	next := len(reqs)
	var delayTimer *time.Timer
	waitDelayTimeout := func() <-chan time.Time {
		if delayTimer == nil {
			return nil
		}
		return delayTimer.C
	}
	if delay != 0 {
		delayTimer = time.NewTimer(delay)
		next = 1
	}

	chans, cancel := prepare(ctx, reqs[:next], handlers[:next])
	defer cancel()

	RespErrs := make([]*RespErr, len(reqs))

	more := func() bool {
		return next != len(reqs)
	}
	fastTriggerNext := func() {
		//if trigger exists, not expired and there remains untriggered Handler, let it run immediately
		if delayTimer != nil && delayTimer.Stop() && more() {
			delayTimer.Reset(0)
		}
	}
	//common case speed up
	var ok bool
	var chosen int
	for {
		if c.breakLoop(RespErrs) {
			break
		}
		var RespErr RespErr
		//select: listen on both callees's channel returns and caller's context cancel.
		select {
		case <-waitDelayTimeout():
			if more() {
				//run next Handler, SELECT on it, prepare next trigger if there remains
				handlerChans, handlerCancel := prepare(ctx, reqs[next:next+1], handlers[next:next+1])
				defer handlerCancel()
				chans = append(chans, handlerChans...)
				next++
				if more() {
					delayTimer.Reset(delay)
				}
			}
			continue
		//getChan return nil for case i > len(chans), block forever. So it works for len(reqs) <= 4
		case RespErr, ok = <-getChan(chans, 0):
			chosen = 0
		case RespErr, ok = <-getChan(chans, 1):
			chosen = 1
		case RespErr, ok = <-getChan(chans, 2):
			chosen = 2
		case RespErr, ok = <-getChan(chans, 3):
			chosen = 3
		case <-ctx.Done():
			return RespErrs, ctx.Err()
		}
		//unregister from select(https://golang.org/ref/spec#Select_statements)
		chans[chosen] = nil
		if !ok {
			panic("never happened")
		}
		RespErrs[chosen] = &RespErr
		if c.earlyReturn(RespErr) {
			return RespErrs, RespErr.Err
		}
		//if last one failed, faster trigger next Handler
		if chosen+1 == next {
			fastTriggerNext()
		}
	}
	return RespErrs, nil
}

var useSingleChan = false

// Top down goroutine call.
// bottom up channel return.
// Asynchronously top down context cancel which Require callee’s explicitly listening.
func wait(ctx context.Context, reqs []Req, handlers []Handler, c waitController, delay time.Duration) ([]*RespErr, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if len(reqs) != len(handlers) {
		return nil, fmt.Errorf("len(reqs) != len(handlers)")
	}
	//get Timeout with default timeout 4 seconds.
	ctx, cancel := setDefaultTimeout(ctx, 4*time.Second)
	defer cancel()

	if useSingleChan {
		return waitSingleChan(ctx, reqs, handlers, c, delay)
	}
	if len(reqs) <= 4 {
		return waitPerReqChanConst(ctx, reqs, handlers, c, delay)
	}
	return waitPerReqChan(ctx, reqs, handlers, c, delay)
}

// WaitAnyResult format the result for waitAny*
func WaitAnyResult(RespErrs []*RespErr, err error) (Resp, int, error) {
	var zero Resp
	if err != nil {
		return zero, -1, err
	}
	for i, resp := range RespErrs {
		if resp != nil && resp.Err == nil {
			return resp.Result, i, nil
		}
	}
	return zero, -1, errors.New("all Request failed")
}

//WaitAny wait until any Request succeeded or all failed
func WaitAny(ctx context.Context, reqs []Req, handlers []Handler) ([]*RespErr, error) {
	return wait(ctx, reqs, handlers, waitAnyControllerConst, 0)
}

// WaitAnyPrefer wait until any Request succeeded or all failed, but run each Handler with delay
// e.g. run first Handler, wait some time(delay),
// if Handler complete first. if Handler success, return, else run next Handler.
// if delay timeout first, run next Handler, wait on both Handler now.
func WaitAnyPrefer(ctx context.Context, reqs []Req, handlers []Handler, delay time.Duration) ([]*RespErr, error) {
	return wait(ctx, reqs, handlers, waitAnyControllerConst, delay)
}

//WaitAnyPrio wait until any Requests succeeded where all high priority Requests are failed.
func WaitAnyPrio(ctx context.Context, reqs []Req, handlers []Handler) ([]*RespErr, error) {
	return wait(ctx, reqs, handlers, waitPrioControllerConst, 0)
}

// WaitAllResult format the result for waitAll, if failed return the first error occurs
func WaitAllResult(RespErrs []*RespErr, err error) ([]Resp, error) {
	if err != nil {
		return nil, err
	}
	var resps = make([]Resp, len(RespErrs))
	var hasNil = false
	for i, resp := range RespErrs {
		if resp != nil {
			if resp.Err != nil {
				return nil, resp.Err
			}
			resps[i] = resp.Result
		} else {
			hasNil = true
		}
	}
	//never happened
	if hasNil {
		return nil, errors.New("some requsets failed to return")
	}
	return resps, nil
}

//WaitAll wait until all Request succeeded or any Request failed
func WaitAll(ctx context.Context, reqs []Req, handlers []Handler) ([]*RespErr, error) {
	return wait(ctx, reqs, handlers, waitAllControllerConst, 0)
}

// WaitAllAndGather wait all and Gather
func WaitAllAndGather(ctx context.Context, reqs []Req, handlers []Handler, g Gather) (Resp, error) {
	ctx, cancel := setDefaultTimeout(ctx, 4*time.Second)
	defer cancel()
	RespErrs, err := WaitAll(ctx, reqs, handlers)
	if err != nil {
		return 0, err
	}

	resps := make([]Resp, len(RespErrs))
	for i, RespErr := range RespErrs {
		resps[i] = RespErr.Result
	}

	return g(ctx, resps)
}

func sum(ctx context.Context, r []int) (int, error) {
	deadline, _ := ctx.Deadline()
	timeout := deadline.Sub(time.Now())
	processTime := timeout * time.Duration(rand.Intn(1.2*100)) / 100
	log.Printf("[sum] Req: %+v, timeout: %s, process time: %s\n", r, timeout, processTime)
	s := 0
	//do things in multi-stage style, check cancel signal at each stage.
	for i, v := range r {
		t := time.NewTimer(processTime / time.Duration(len(r)))
		defer t.Stop()
		select {
		case <-t.C:
			//random process time and random error
			if rand.Intn(10) == 0 {
				return 0, fmt.Errorf("[sum] Req: %+v process failed", r)
			}
			s += v
		case <-ctx.Done():
			log.Printf("[context] Req: %+v, stage: %d, error: %+v\n", r, i+1, ctx.Err())
			return 0, ctx.Err()
		}
	}
	return s, nil
}

// run (1+2), (3+4), (5+6) simultaneously, wait all their returns (3, 7, 11), sum them up(3+7+11)
func waitAllAndGatherTest() {
	sumHandler := func(ctx context.Context, r Req) (Resp, error) {
		ints := r.([]int)
		return sum(ctx, ints)
	}
	sumGather := func(ctx context.Context, resps []Resp) (Resp, error) {
		ints := make([]int, len(resps))
		for i, r := range resps {
			ints[i] = r.(int)
		}
		return sum(ctx, ints)
	}
	result, err := WaitAllAndGather(context.Background(),
		[]Req{Req([]int{1, 2}), Req([]int{3, 4}), Req([]int{5, 6})},
		[]Handler{sumHandler, sumHandler, sumHandler},
		sumGather,
	)
	if err != nil {
		return
	}
	i := result.(int)
	log.Printf("result: %d", i)
}

// run (1+2), (3+4), (5+6) simultaneously, return first one succeeded(3 or 7 or 11)
func waitAnyTest() {
	sumHandler := func(ctx context.Context, r Req) (Resp, error) {
		ints := r.([]int)
		return sum(ctx, ints)
	}
	reqs := []Req{Req([]int{1, 2}), Req([]int{3, 4}), Req([]int{5, 6})}
	resp, i, err := WaitAnyResult(WaitAnyPrefer(context.Background(),
		reqs,
		[]Handler{sumHandler, sumHandler, sumHandler},
		time.Second,
	))
	if err != nil {
		log.Println(err)
		return
	}
	log.Printf("waitAny reqs: %+v, Req:%+v success, resp: %+v\n", reqs, reqs[i], resp)
}

func main() {
	rand.Seed(time.Now().UnixNano()) //default rand source is goroutine safe
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
	for i := 0; i < 100; i++ {
		waitAnyTest()
		time.Sleep(3 * time.Second)
		fmt.Println("")
		waitAllAndGatherTest()
		time.Sleep(3 * time.Second)
		fmt.Println("")
		//wait all goroutine to finish.
	}
}
