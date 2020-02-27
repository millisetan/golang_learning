package main

func calling(a int, b int) (c int, d int, e int) {
	var k = 10
	return a, b, k
}

func main() {
	calling(12, 13)
}
