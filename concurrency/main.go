package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"reflect"
	"runtime"
	"time"
)

func setDefaultTimeout(ctx context.Context, t time.Duration) (context.Context, context.CancelFunc) {
	var cancel context.CancelFunc = func() { log.Fatal("fuck") }
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

type req interface{}
type resp interface{}

type respErr struct {
	val resp
	err error
}

func getHandlerName(h handler) string {
	return runtime.FuncForPC(reflect.ValueOf(h).Pointer()).Name()
}

type handler func(ctx context.Context, r req) (resp, error)
type gather func(ctx context.Context, resps []resp) (resp, error)

func getChan(chans []<-chan respErr, i int) <-chan respErr {
	if i < len(chans) {
		return chans[i]
	}
	return nil
}

//run handler simultaneously, wait for response and cancel signal
func doHandleSub(ctx context.Context, c chan<- respErr, r req, h handler) {
	//close c in only sender, indicates no more data can be read, needless here.
	defer close(c)
	waitResponse := func() <-chan respErr {
		c := make(chan respErr)
		go func() {
			defer close(c)
			var resp respErr
			defer func() { c <- resp }()
			resp.val, resp.err = h(ctx, r)
		}()
		return c
	}
	select {
	case resp, _ := <-waitResponse():
		log.Printf("[process] req: %+v, return: %+v, error: %+v\n", r, resp.val, resp.err)
		c <- resp
	case <-ctx.Done():
		log.Printf("[context] req: %+v, error: %+v\n", r, ctx.Err())
		c <- respErr{0, ctx.Err()}
	}
}

//prepare channel response
func doHandle(ctx context.Context, r req, h handler) <-chan respErr {
	c := make(chan respErr, 1)
	//take care when pass by reference!!
	go doHandleSub(ctx, c, r, h)
	return c
}

//set up channel and context communication with handler
func prepare(ctx context.Context, reqs []req, handlers []handler) ([]<-chan respErr, context.CancelFunc) {
	cancels := make([]context.CancelFunc, len(reqs))
	cancel := func() {
		for _, cancel := range cancels {
			cancel()
		}
	}
	chans := make([]<-chan respErr, len(reqs))
	for i, req := range reqs {
		reqCtx, reqCancel := context.WithTimeout(ctx, getProcessTimeout(ctx))
		cancels[i] = reqCancel
		chans[i] = doHandle(reqCtx, req, handlers[i])
	}
	return chans, cancel
}

type waitController interface {
	//early return when recv response
	earlyReturn(respErr) bool
	//check if loop is over
	breakLoop([]*respErr) bool
}

func waitAnyResult(respErrs []*respErr, err error) (respErr, int, error) {
	var zero respErr
	if err != nil {
		return zero, -1, err
	}
	for i, resp := range respErrs {
		if resp != nil && resp.err == nil {
			return *resp, i, nil
		}
	}
	return zero, -1, errors.New("all request failed")
}

type waitAnyController int

const waitAnyControllerConst = waitAnyController(0)

//any request succeeded
func (waitAnyController) earlyReturn(r respErr) bool {
	return r.err == nil
}

//all failed
func (waitAnyController) breakLoop(resps []*respErr) bool {
	for _, resp := range resps {
		if resp == nil {
			return false
		}
	}
	return true
}

//wait until any request succeeded or all failed
func waitAny(ctx context.Context, reqs []req, handlers []handler) (respErr, int, error) {
	return waitAnyResult(wait(ctx, reqs, handlers, waitAnyControllerConst, 0))
}

func waitAnyPrefer(ctx context.Context, reqs []req, handlers []handler, delay time.Duration) (respErr, int, error) {
	return waitAnyResult(wait(ctx, reqs, handlers, waitAnyControllerConst, delay))
}

type waitPrioController int

const waitPrioControllerConst = waitPrioController(0)

//useless here, waitprio need global info, highest priority succeeded may early return
func (waitPrioController) earlyReturn(r respErr) bool {
	return false
}

//wait priority succeeded or all failed
func (waitPrioController) breakLoop(resps []*respErr) bool {
	allFailures := true
	for _, resp := range resps {
		if resp != nil {
			if resp.err == nil {
				return true
			}
		} else {
			allFailures = false
		}
	}
	return allFailures
}

//wait until any requests succeeded where all high priority requests are failed.
func waitAnyPrio(ctx context.Context, reqs []req, handlers []handler) (respErr, int, error) {
	return waitAnyResult(wait(ctx, reqs, handlers, waitPrioControllerConst, 0))
}

type waitAllController int

const waitAllControllerConst = waitAllController(0)

func (waitAllController) earlyReturn(r respErr) bool {
	return r.err != nil
}
func (waitAllController) breakLoop(resps []*respErr) bool {
	for _, resp := range resps {
		if resp == nil {
			return false
		}
	}
	return true
}

//wait until all request succeeded or any request failed
func waitAll(ctx context.Context, reqs []req, handlers []handler) ([]*respErr, error) {
	return wait(ctx, reqs, handlers, waitAllControllerConst, 0)
}

func wait(ctx context.Context, reqs []req, handlers []handler, c waitController, delay time.Duration) ([]*respErr, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if len(reqs) != len(handlers) {
		return nil, fmt.Errorf("len(reqs) != len(handlers)")
	}
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

	respErrs := make([]*respErr, len(reqs))
	/*
		if len(chans) <= 1 {
			//common case speed up
			for {
				if c.breakLoop(respErrs) {
					break
				}
				//select: listen on both callees's channel returns and caller's context cancel.
				select {
				case RespErr, ok := <-getChan(chans, 0):
					//unregister from select(https://golang.org/ref/spec#Select_statements)
					chans[0] = nil
					if !ok {
						panic("never happened")
					}
					respErrs[0] = &RespErr
					if c.earlyReturn(RespErr) {
						fmt.Println("xx")
						return respErrs, RespErr.err
					}
				case <-ctx.Done():
					return respErrs, ctx.Err()
				}
			}
		} else
	*/
	{
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
			if delayTimer != nil && delayTimer.Stop() {
				if next != len(reqs) {
					delayTimer.Reset(0)
				}
			}
		}
		for {
			if c.breakLoop(respErrs) {
				break
			}
			chosen, recv, recvOK := reflect.Select(cases)
			switch chosen {
			case 0:
				return respErrs, ctx.Err()
			case 1:
				if more() {
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
					fmt.Println(chosen)
					panic("never happened1")
				}
				RespErr, ok := recv.Interface().(respErr)
				if !ok {
					panic("never happened")
				}
				respErrs[chosen] = &RespErr
				if c.earlyReturn(RespErr) {
					return respErrs, RespErr.err
				}
				fastTriggerNext()
			}
		}
	}
	for _, resp := range respErrs {
		if resp != nil && resp.err == nil {
			return respErrs, nil
		}
	}
	return respErrs, errors.New("all failed")
}

/*
	Top down goroutine call.
	bottom up channel return.
	Asynchronously top down context cancel which require callee’s explicitly listening.
*/

func concurrent(ctx context.Context, reqs []req, handlers []handler, g gather) (resp, error) {
	//get Timeout with default timeout 4 seconds.
	ctx, cancel := setDefaultTimeout(ctx, 5*time.Second)
	defer cancel()
	// if timeout < min processing time, return immediately
	respErrs, err := waitAll(ctx, reqs, handlers)
	if err != nil {
		return 0, err
	}

	resps := make([]resp, 0, len(respErrs))
	for _, respErr := range respErrs {
		if respErr != nil && respErr.err == nil {
			resps = append(resps, respErr.val)
		}
	}

	return g(ctx, resps)
}

//do things in multi-stage style, check cancel signal at each stage.
func sum(ctx context.Context, r []int) (int, error) {
	deadline, _ := ctx.Deadline()
	timeout := deadline.Sub(time.Now())
	processTime := timeout * time.Duration(rand.Intn(1.2*100)) / 100 * 10000
	log.Printf("[sum] req: %+v, timeout: %s, process time: %s\n", r, timeout, processTime)
	s := 0
	for i, v := range r {
		t := time.NewTimer(processTime / time.Duration(len(r)))
		defer t.Stop()
		select {
		case <-t.C:
			if rand.Intn(10) == 100 {
				return 0, fmt.Errorf("[sum] req: %+v process failed", r)
			}
			s += v
		case <-ctx.Done():
			log.Printf("[context] req: %+v, stage: %d, error: %+v\n", r, i+1, ctx.Err())
			return 0, ctx.Err()
		}
	}
	return s, nil
}

/*
	((1+2)+(3+4)+(5+6))
	  ---   ---   ---
	   3  +  7  +  11
	 -----------------
			 21
	run (1+2), (3+4), (5+6) simultaneously, wait all their returns (3, 7, 11) and sum them up together(3+7+11)
*/
func concurrentSum() {
	sumHandler := func(ctx context.Context, r req) (resp, error) {
		ints := r.([]int)
		return sum(ctx, ints)
	}
	sumGather := func(ctx context.Context, resps []resp) (resp, error) {
		ints := make([]int, len(resps))
		for i, r := range resps {
			ints[i] = r.(int)
		}
		return sum(ctx, ints)
	}
	result, err := concurrent(context.Background(),
		[]req{req([]int{1, 2}), req([]int{3, 4}), req([]int{5, 6})},
		[]handler{sumHandler, sumHandler, sumHandler},
		sumGather,
	)
	if err != nil {
		log.Println(err)
		return
	}
	i := result.(int)
	log.Printf("concurrentSum result: %d", i)
}

func main() {
	rand.Seed(time.Now().UnixNano()) //default rand source is goroutine safe
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
	concurrentSum()
	time.Sleep(5 * time.Second)
}
