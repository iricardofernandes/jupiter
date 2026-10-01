//go:build simulation

package simulation_test

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// seeds are the runs to make: SIM_SEED, one seed to replay; else SIM_SEEDS, a list
// ("1,2,3"), a range ("1-5") or random ones ("random:4"); else one random seed. CI runs
// a fixed range on every push; the nightly run, random seeds.
func seeds(t *testing.T) []uint64 {
	t.Helper()
	if seed := envUint("SIM_SEED", 0); seed != 0 {
		return []uint64{seed}
	}
	spec := os.Getenv("SIM_SEEDS")
	if spec == "" {
		spec = "random:1"
	}
	var out []uint64
	for part := range strings.SplitSeq(spec, ",") {
		got, err := parseSeeds(strings.TrimSpace(part))
		if err != nil {
			t.Fatalf("SIM_SEEDS=%q: %v", spec, err)
		}
		out = append(out, got...)
	}
	return out
}

func parseSeeds(part string) ([]uint64, error) {
	if n, ok := strings.CutPrefix(part, "random:"); ok {
		count, err := strconv.Atoi(n)
		if err != nil || count < 1 || count > 1000 {
			return nil, fmt.Errorf("%q is not a count of seeds", n)
		}
		out := make([]uint64, count)
		for i := range out {
			var b [8]byte
			_, _ = rand.Read(b[:])
			out[i] = binary.LittleEndian.Uint64(b[:]) | 1
		}
		return out, nil
	}
	if from, to, ok := strings.Cut(part, "-"); ok {
		a, errA := strconv.ParseUint(from, 10, 64)
		b, errB := strconv.ParseUint(to, 10, 64)
		if errA != nil || errB != nil || a == 0 || b < a || b-a >= 1000 {
			return nil, fmt.Errorf("%q is not a range of seeds", part)
		}
		var out []uint64
		for s := a; s <= b; s++ {
			out = append(out, s)
		}
		return out, nil
	}
	seed, err := strconv.ParseUint(part, 10, 64)
	if err != nil || seed == 0 {
		return nil, fmt.Errorf("%q is not a seed", part)
	}
	return []uint64{seed}, nil
}

// Every seed that once found a bug runs again on every push, at the size that found it:
// testdata/regressions lists them, one a line, as "<simulation> <seed> <size> # what it
// found", the simulation being cards or rails.
func TestRegressionSeeds(t *testing.T) {
	f, err := os.Open("testdata/regressions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		line, _, _ := strings.Cut(lines.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			t.Fatalf("testdata/regressions: %q", lines.Text())
		}
		seed, errSeed := strconv.ParseUint(fields[1], 10, 64)
		size, errSize := strconv.Atoi(fields[2])
		if errSeed != nil || errSize != nil || size < 1 {
			t.Fatalf("testdata/regressions: %q", lines.Text())
		}
		t.Run(fields[0]+"/"+fields[1], func(t *testing.T) {
			switch fields[0] {
			case "cards":
				newSim(t, seed).run(size)
			case "rails":
				newRails(t, seed).run(size)
			default:
				t.Fatalf("testdata/regressions: no simulation %q", fields[0])
			}
		})
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
}
