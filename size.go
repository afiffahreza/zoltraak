package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// sizeFlag is a flag.Value that stores a byte count in *p. It accepts a plain
// integer ("1048576") or a number with a unit suffix ("1M", "512K", "1.5GiB").
type sizeFlag struct{ p *int64 }

func newSizeFlag(p *int64, def int64) *sizeFlag {
	*p = def
	return &sizeFlag{p}
}

func (s *sizeFlag) Set(v string) error {
	n, err := parseSize(v)
	if err != nil {
		return err
	}
	*s.p = n
	return nil
}

// String renders exact binary multiples compactly ("1M"), otherwise plain bytes.
func (s *sizeFlag) String() string {
	if s == nil || s.p == nil {
		return "0"
	}
	n := *s.p
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if n >= u.mult && n%u.mult == 0 {
			return strconv.FormatInt(n/u.mult, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// sizeUnits maps a lower-cased suffix to its multiplier. K/M/G/T and the
// KiB/MiB/... forms are powers of 1024; KB/MB/... are powers of 1000.
var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
}

// parseSize parses a non-negative byte count such as "1048576", "1M", "1.5GiB"
// or "200mb". Whole numbers without a fraction are parsed exactly.
func parseSize(s string) (int64, error) {
	in := strings.TrimSpace(s)
	i := 0
	for i < len(in) && (in[i] >= '0' && in[i] <= '9' || in[i] == '.') {
		i++
	}
	num, unit := in[:i], strings.ToLower(strings.TrimSpace(in[i:]))
	mult, ok := sizeUnits[unit]
	if num == "" || !ok {
		return 0, fmt.Errorf("invalid size %q (use bytes or a K/M/G/T suffix, e.g. 1048576, 512K, 1M, 1.5G)", s)
	}
	if !strings.Contains(num, ".") {
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil || n > int64(math.MaxInt64/mult) {
			return 0, fmt.Errorf("size %q is too large", s)
		}
		return n * int64(mult), nil
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f*mult >= math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * mult), nil
}
