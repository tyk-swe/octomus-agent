package jsoncompat

import (
	"fmt"
	"math"
	"strconv"
)

// parseSerdeFloat follows serde_json 1.0.151 without float_roundtrip, the parser
// the frozen Rust reference used. Its bounded integer significand and subsequent f64 scaling
// can round differently from strconv.ParseFloat on the whole decimal string.
// s has already passed encoding/json's syntax validation.
func parseSerdeFloat(s string) (float64, error) {
	negative := s[0] == '-'
	i := 0
	if negative {
		i++
	}
	var significand uint64
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		digit := uint64(s[i] - '0')
		if significand > (math.MaxUint64-digit)/10 {
			break
		}
		significand = significand*10 + digit
		i++
	}
	// Integer digits beyond u64 precision increase the decimal exponent.
	var exponent int64
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		exponent++
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			digit := uint64(s[i] - '0')
			if significand > (math.MaxUint64-digit)/10 {
				break
			}
			significand = significand*10 + digit
			exponent--
			i++
		}
		// Fractional digits beyond u64 precision are discarded, without
		// rounding or changing the exponent.
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i < len(s) { // e or E; syntax validation guarantees exponent digits.
		i++
		negativeExponent := s[i] == '-'
		if s[i] == '-' || s[i] == '+' {
			i++
		}
		var explicitExponent int64
		for ; i < len(s); i++ {
			digit := int64(s[i] - '0')
			if explicitExponent > (math.MaxInt32-digit)/10 {
				if !negativeExponent && significand != 0 {
					return 0, fmt.Errorf("JSON number out of range")
				}
				if negative {
					return math.Copysign(0, -1), nil
				}
				return 0, nil
			}
			explicitExponent = explicitExponent*10 + digit
		}
		if negativeExponent {
			exponent -= explicitExponent
		} else {
			exponent += explicitExponent
		}
		exponent = max(math.MinInt32, min(math.MaxInt32, exponent))
	}

	n := float64(significand)
	// Rust divides in 1e308 steps for tiny values, preserving subnormals and
	// signed zero instead of overflowing a single scaling factor.
	for exponent < -308 && n != 0 {
		n /= 1e308
		exponent += 308
	}
	if n != 0 {
		if exponent > 308 {
			return 0, fmt.Errorf("JSON number out of range")
		}
		if exponent < 0 {
			n /= serdePowersOfTen[-exponent]
		} else {
			n *= serdePowersOfTen[exponent]
		}
		if math.IsInf(n, 0) {
			return 0, fmt.Errorf("JSON number out of range")
		}
	}
	if negative {
		n = -n
	}
	return n, nil
}

// serde's POW10 table uses individually rounded f64 literals. math.Pow10
// multiplies rounded factors and can differ by an ULP, so parse each power once.
var serdePowersOfTen = func() (powers [309]float64) {
	for exponent := range powers {
		powers[exponent], _ = strconv.ParseFloat("1e"+strconv.Itoa(exponent), 64)
	}
	return
}()
