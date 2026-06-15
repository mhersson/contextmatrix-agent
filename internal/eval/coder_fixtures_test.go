package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCoderFixturesAreValid asserts every added coder fixture is a real, solvable
// task: the shipped skeleton FAILS its hidden test, and a reference implementation
// makes it PASS. A fixture that passes unimplemented (no signal) or can never pass
// (broken test) would fail here.
func TestCoderFixturesAreValid(t *testing.T) {
	cases := []struct {
		fixture  string
		implFile string
		impl     string
		check    string // "" defaults to "go test ./..."
	}{
		{
			fixture:  "fixtures/coder/reverse",
			implFile: "reverse.go",
			impl:     "package reverse\n\nfunc Reverse(s string) string {\n\tr := []rune(s)\n\tfor i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {\n\t\tr[i], r[j] = r[j], r[i]\n\t}\n\treturn string(r)\n}\n",
		},
		{
			fixture:  "fixtures/coder/fizzbuzz",
			implFile: "fizzbuzz.go",
			impl:     "package fizzbuzz\n\nimport \"strconv\"\n\nfunc FizzBuzz(n int) []string {\n\tout := make([]string, 0, n)\n\tfor i := 1; i <= n; i++ {\n\t\tswitch {\n\t\tcase i%15 == 0:\n\t\t\tout = append(out, \"FizzBuzz\")\n\t\tcase i%3 == 0:\n\t\t\tout = append(out, \"Fizz\")\n\t\tcase i%5 == 0:\n\t\t\tout = append(out, \"Buzz\")\n\t\tdefault:\n\t\t\tout = append(out, strconv.Itoa(i))\n\t\t}\n\t}\n\treturn out\n}\n",
		},
		{
			fixture:  "fixtures/coder/dedup",
			implFile: "dedup.go",
			impl:     "package dedup\n\nfunc Dedup(xs []int) []int {\n\tvar out []int\n\tseen := map[int]bool{}\n\tfor _, x := range xs {\n\t\tif !seen[x] {\n\t\t\tseen[x] = true\n\t\t\tout = append(out, x)\n\t\t}\n\t}\n\treturn out\n}\n",
		},
		{
			fixture:  "fixtures/coder/stats",
			implFile: "stats.go",
			impl:     "package stats\n\nfunc Max(xs []int) int {\n\tif len(xs) == 0 {\n\t\treturn 0\n\t}\n\tm := xs[0]\n\tfor _, x := range xs {\n\t\tif x > m {\n\t\t\tm = x\n\t\t}\n\t}\n\treturn m\n}\n",
		},
		{
			fixture:  "fixtures/coder/meanfloor",
			implFile: "meanfloor.go",
			impl: `package meanfloor

import "math/big"

func MeanFloor(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	sum := new(big.Int)
	for _, x := range xs {
		sum.Add(sum, big.NewInt(x))
	}
	// Div is Euclidean (floor for a positive divisor); Quo would truncate toward zero.
	q := new(big.Int).Div(sum, big.NewInt(int64(len(xs))))
	return q.Int64()
}
`,
		},
		{
			fixture:  "fixtures/coder/sumlist",
			implFile: "sumlist.go",
			impl: `package sumlist

func Sum(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}
`,
		},
		{
			fixture:  "fixtures/coder/safecount",
			implFile: "safecount.go",
			check:    "CGO_ENABLED=1 go test -race ./...",
			impl: `package safecount

import "sync"

type Counter struct {
	mu     sync.Mutex
	counts map[string]int
}

func New() *Counter {
	return &Counter{counts: make(map[string]int)}
}

func (c *Counter) Inc(key string) {
	c.mu.Lock()
	c.counts[key]++
	c.mu.Unlock()
}

func (c *Counter) Get(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}
`,
		},
		{
			fixture:  "fixtures/coder/lru",
			implFile: "lru.go",
			impl: `package lru

type Cache struct {
	cap   int
	items map[int]*node
	order *list
}

// New returns an empty cache that holds at most capacity entries (capacity >= 1).
func New(capacity int) *Cache {
	return &Cache{cap: capacity, items: make(map[int]*node), order: newList()}
}

func (c *Cache) Get(key int) (int, bool) {
	n, ok := c.items[key]
	if !ok {
		return 0, false
	}
	c.order.moveToFront(n)
	return n.value, true
}

func (c *Cache) Put(key, value int) {
	if n, ok := c.items[key]; ok {
		n.value = value
		c.order.moveToFront(n)
		return
	}
	n := &node{key: key, value: value}
	c.items[key] = n
	c.order.pushFront(n)
	if len(c.items) > c.cap {
		victim := c.order.oldest()
		c.order.remove(victim)
		delete(c.items, victim.key)
	}
}
`,
		},
		{
			fixture:  "fixtures/coder/truncate",
			implFile: "truncate.go",
			impl: `package truncate

import "unicode/utf8"

func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}
`,
		},
		{
			fixture:  "fixtures/coder/calc",
			implFile: "calc.go",
			impl: `package calc

import (
	"errors"
	"strconv"
	"strings"
)

func Eval(expr string) (int, error) {
	p := &parser{s: strings.TrimSpace(expr)}
	if p.s == "" {
		return 0, errors.New("empty expression")
	}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.pos != len(p.s) {
		return 0, errors.New("unexpected trailing input")
	}
	return v, nil
}

type parser struct {
	s   string
	pos int
}

func (p *parser) skipSpaces() {
	for p.pos < len(p.s) && p.s[p.pos] == ' ' {
		p.pos++
	}
}

func (p *parser) parseExpr() (int, error) {
	v, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpaces()
		if p.pos >= len(p.s) {
			break
		}
		op := p.s[p.pos]
		if op != '+' && op != '-' {
			break
		}
		p.pos++
		rhs, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			v += rhs
		} else {
			v -= rhs
		}
	}
	return v, nil
}

func (p *parser) parseTerm() (int, error) {
	v, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpaces()
		if p.pos >= len(p.s) {
			break
		}
		op := p.s[p.pos]
		if op != '*' && op != '/' {
			break
		}
		p.pos++
		rhs, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		if op == '*' {
			v *= rhs
		} else {
			if rhs == 0 {
				return 0, errors.New("division by zero")
			}
			v /= rhs
		}
	}
	return v, nil
}

func (p *parser) parseFactor() (int, error) {
	p.skipSpaces()
	if p.pos >= len(p.s) {
		return 0, errors.New("unexpected end of input")
	}
	c := p.s[p.pos]
	if c == '(' {
		p.pos++
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		p.skipSpaces()
		if p.pos >= len(p.s) || p.s[p.pos] != ')' {
			return 0, errors.New("missing closing paren")
		}
		p.pos++
		return v, nil
	}
	if c >= '0' && c <= '9' {
		start := p.pos
		for p.pos < len(p.s) && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
			p.pos++
		}
		n, err := strconv.Atoi(p.s[start:p.pos])
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, errors.New("unexpected character")
}
`,
		},
		{
			fixture:  "fixtures/coder/ratelimit",
			implFile: "ratelimit.go",
			impl: `package ratelimit

type Limiter struct {
	capacity int
	window   int64
	events   []int64
}

func NewLimiter(capacity int, window int64) *Limiter {
	return &Limiter{capacity: capacity, window: window}
}

func (l *Limiter) Allow(now int64) bool {
	cutoff := now - l.window
	kept := l.events[:0]
	for _, t := range l.events {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	l.events = kept
	if len(l.events) >= l.capacity {
		return false
	}
	l.events = append(l.events, now)
	return true
}
`,
		},
	}
	for _, c := range cases {
		t.Run(filepath.Base(c.fixture), func(t *testing.T) {
			check := c.check
			if check == "" {
				check = "go test ./..."
			}

			if strings.Contains(check, "-race") && !cgoAvailable() {
				check = "go test ./..."
			}

			ct := CoderTask{name: filepath.Base(c.fixture), fixture: c.fixture, check: check, writable: []string{c.implFile}}
			dir := t.TempDir()
			require.NoError(t, ct.Provision(dir))

			v, err := ct.Check(context.Background(), dir, harnessZero())
			require.NoError(t, err)
			assert.False(t, v.OK, "shipped skeleton should FAIL its test: %s", v.Detail)

			require.NoError(t, os.WriteFile(filepath.Join(dir, c.implFile), []byte(c.impl), 0o644))
			v, err = ct.Check(context.Background(), dir, harnessZero())
			require.NoError(t, err)
			assert.True(t, v.OK, "reference impl should PASS: %s", v.Detail)
		})
	}
}

// cgoAvailable reports whether a C toolchain is present, used as a heuristic for
// whether `go test -race` will work. It assumes a glibc toolchain; on a musl host
// (e.g. Alpine) with gcc installed it may report true even where the race detector
// cannot link. Race-checked fixtures fall back to a plain test run when it is false.
func cgoAvailable() bool {
	for _, cc := range []string{"gcc", "cc", "clang"} {
		if _, err := exec.LookPath(cc); err == nil {
			return true
		}
	}

	return false
}
