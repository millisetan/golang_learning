package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"runtime"
	"time"
)

func getCaller() string {
	pc, _, _, ok := runtime.Caller(3)
	if !ok {
		return "[unknown] func"
	}
	details := runtime.FuncForPC(pc)
	if details == nil {
		return "[unknown] func"
	}
	return details.Name()
}

func add(ctx context.Context, a, b int, ints ...int) (int, error) {
	rand.Seed(time.Now().UnixNano()) //default rand source is goroutine safe
	//always has deadline
	deadline, _ := ctx.Deadline()
	timeout := deadline.Sub(time.Now())
	processTime := timeout * time.Duration(rand.Intn(1.2*100)) / 100
	log.Printf("func: %s, deadline: %+v, process time: %s\n", getCaller(), timeout, processTime)
	t := time.NewTimer(processTime)
	defer t.Stop()
	sum := func(a, b int, ints ...int) int {
		t := a + b
		for _, x := range ints {
			t += x
		}
		return t
	}
	select {
	case <-t.C:
		if rand.Intn(10) == 0 {
			return 0, fmt.Errorf("func: %s process failed", getCaller())
		}
		return sum(a, b, ints...), nil
	case <-ctx.Done():
		log.Printf("func: %s canceled, error: %+v, resp: %+v\n", getCaller(), ctx.Err(), respErr{0, ctx.Err()})
		return 0, ctx.Err()
	}

}

func doAdd(ctx context.Context, c chan<- respErr, a, b int, ints ...int) {
	//close c in sender, indicates no more data can be read, needless here.
	defer close(c)
	//always send
	var resp respErr
	defer func() { c <- resp }()
	resp.val, resp.err = add(ctx, a, b, ints...)
	return
}

func b(ctx context.Context, c chan<- respErr) {
	doAdd(ctx, c, 1, 2)
	return
}

func c(ctx context.Context, c chan<- respErr) {
	doAdd(ctx, c, 3, 4)
	return
}

func d(ctx context.Context, c chan<- respErr) {
	doAdd(ctx, c, 5, 6)
	return
}

type respErr struct {
	val int
	err error
}

/*
	((1+2)+(3+4)+(5+6))
	  ---   ---   ---
	   b     c     d
	 -----------------
	         a
	Top down goroutine call.
	bottom up channel return.
	Asynchronously top down context cancel which require callee’s explicitly listening.
*/
func a(ctx context.Context) (int, error) {
	//add default Deadline, wait 4 seconds.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	timeout := deadline.Sub(time.Now())
	// if timeout < min processing time, return immediately

	bChan := make(chan respErr, 1)
	var bVal int
	//empirical value 1/2
	bctx, bcancel := context.WithTimeout(ctx, timeout/2)
	//alway cancel
	defer bcancel()
	//take care when pass by reference!!
	go b(bctx, bChan)

	cChan := make(chan respErr, 1)
	var cVal int
	cctx, ccancel := context.WithTimeout(ctx, timeout/2)
	defer ccancel()
	go c(cctx, cChan)

	dChan := make(chan respErr, 1)
	var dVal int
	dctx, dcancel := context.WithTimeout(ctx, timeout/2)
	defer dcancel()
	go d(dctx, dChan)

	for {
		//select: listen on both callees's channel returns and caller's context cancel.
		select {
		case RespErr, ok := <-bChan:
			log.Printf("func: main.b done, resp: %+v, ok: %+v\n", RespErr, ok)
			/*
				unregister bChan from select, reflect select can be more powerful with penalty cost(readability & performance)
				#https://golang.org/ref/spec#Select_statements
				#https://golang.org/pkg/reflect/#Select
			*/
			bChan = nil
			if !ok {
				panic("never happened")
			}
			if RespErr.err != nil {
				//early return
				return 0, RespErr.err
			}
			bVal = RespErr.val
		case RespErr, ok := <-cChan:
			log.Printf("func: main.c done, resp: %+v, ok: %+v\n", RespErr, ok)
			cChan = nil
			if !ok {
				panic("never happened")
			}
			if RespErr.err != nil {
				return 0, RespErr.err
			}
			cVal = RespErr.val
		case RespErr, ok := <-dChan:
			log.Printf("func: main.d done, resp: %+v, ok: %+v\n", RespErr, ok)
			dChan = nil
			if !ok {
				panic("never happened")
			}
			if RespErr.err != nil {
				return 0, RespErr.err
			}
			dVal = RespErr.val
		case <-ctx.Done():
			log.Printf("func: %s canceled\n", "main.a")
			return 0, ctx.Err()
		}

		if bChan == nil && cChan == nil && dChan == nil {
			break
		}
	}

	//process
	var resultChan = make(chan respErr, 1)
	doAdd(ctx, resultChan, bVal, cVal, dVal)
	result := <-resultChan
	return result.val, result.err
}

func main() {
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	val, err := a(context.Background())
	if err != nil {
		log.Println(err)
	} else {
		fmt.Println(val)
	}
	time.Sleep(5 * time.Second)
}
