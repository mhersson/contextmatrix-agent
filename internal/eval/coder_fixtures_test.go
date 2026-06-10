package eval

import (
	"context"
	"os"
	"path/filepath"
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
	}
	for _, c := range cases {
		t.Run(filepath.Base(c.fixture), func(t *testing.T) {
			check := c.check
			if check == "" {
				check = "go test ./..."
			}
			ct := CoderTask{name: filepath.Base(c.fixture), fixture: c.fixture, check: check}
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
