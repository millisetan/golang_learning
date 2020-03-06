package main

import (
	"fmt"
	"time"
	"unsafe"
)

//Funcs end with Impl is the implementation of previous functions.

/*
simple calling convention.
arguments and returns are pushed on stack in reverse order(e, d, c, b, a), and call function
|      e
|      d
|      c
|      b
v      a
rsp---

*/
func simpleCall(a int, b int) (c int, d int, e int) {
	return a, b, 10
}

//simple closure, args and variable are shared
func closure(i int) int {
	var j = 10
	func() {
		i = 20
		j = 30
	}()
	return i + j
}

//when closure is called, surrounding function is still on stack, we simple pass pointer of shares as args.
func closureImpl(i int) int {
	var j = 10
	func(i *int, j *int) {
		*i = 20
		*j = 30
	}(&i, &j)
	return i + j
}

//call closure as goroutine where closure share vars and args with surrounding function.
func goClosure(i int) int {
	var j = 10
	go func() {
		i = 20
		j = 30
	}()
	time.Sleep(time.Millisecond)
	return i + j
}

//closure is run on a separate goroutine, whose stack is different from the surrounding function, so we must pass pointer of vars and args.
//the variable must not allocated on stack even it declared to be(thanks GC, it's ok to allocate variable on heap), as we may reference it from another goroutine. Cross stack reference is just asking for trouble(GC).
func goClosureImpl(i int) int {
	ip := new(int)
	*ip = i
	j := new(int)
	*j = 10
	go func(i *int, j *int) {
		*i = 20
		*j = 30
	}(ip, j)
	time.Sleep(time.Millisecond)
	return *ip + *j
}

//things get a bit tough if we pass closure as arg to function.
//Here closureAsArg call doFunc, doFunc call closure which access variable of closureAsArg
//There must be a generic way(compatible with simple function as arg) for closureAsArg and closure to negotiate using doFunc(proxy)
func doFunc(f func()) {
	f()
}

func closureAsArg(i int) int {
	var j = 10
	doFunc(func() {
		i = 20
		j = 30
	})
	return i + j
}

type funcval struct {
	fn uintptr
	// variable-size, fn-specific data here
}

//f is a pointer to funcval struct, fn is the address of a function, funcval may have fn-specific data following it.
//doFuncImpl save funcval in ctx register(rbx in amd64)
func doFuncImpl(fn *funcval) {
	//save context on regitster
	ctxReg = unsafe.Pointer(fn)
	//call closure
	(*(*func())(unsafe.Pointer(fn.fn)))()
}

//save on register is easy in assembly, but it's difficult for golang.
var ctxReg unsafe.Pointer

//
func closureAsArgImpl(i int) int {
	ip := new(int)
	*ip = i
	j := new(int)
	*j = 10
	type thisCtx struct {
		funcval
		i *int
		j *int
	}
	FuncABCmouse := func() {
		//we get thisCtx from ctx register
		ctx := (*thisCtx)(ctxReg)
		*ctx.i = 20
		*ctx.j = 30
	}
	var fn = &thisCtx{
		funcval: funcval{
			fn: uintptr(unsafe.Pointer(&FuncABCmouse)),
		},
		i: ip,
		j: j,
	}
	doFuncImpl((*funcval)(unsafe.Pointer(fn)))
	return *ip + *j
}

func main() {
	fmt.Println(closure(10))
	fmt.Println(closureImpl(10))
	fmt.Println(goClosure(10))
	fmt.Println(goClosureImpl(10))
	fmt.Println(closureAsArg(10))
	fmt.Println(closureAsArgImpl(10))
}
