package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var ErrInvalidAmount = errors.New("invalid USDC amount")

func ParseUSDC(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") {
		return 0, ErrInvalidAmount
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, ErrInvalidAmount
	}
	if _, err := strconv.ParseUint(parts[0], 10, 63); err != nil {
		return 0, ErrInvalidAmount
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > 6 {
		return 0, ErrInvalidAmount
	}
	for len(fraction) < 6 {
		fraction += "0"
	}
	whole, _ := strconv.ParseInt(parts[0], 10, 63)
	frac, _ := strconv.ParseInt(fraction, 10, 64)
	if whole > math.MaxInt64/1_000_000 || whole*1_000_000 > math.MaxInt64-frac {
		return 0, ErrInvalidAmount
	}
	return whole*1_000_000 + frac, nil
}

func FormatUSDC(micro int64) string {
	return fmt.Sprintf("%d.%06d", micro/1_000_000, micro%1_000_000)
}

func MultiplyTokens(tokens int, priceMicro int64) (int64, error) {
	if tokens < 0 || priceMicro < 0 {
		return 0, ErrInvalidAmount
	}
	if priceMicro != 0 && int64(tokens) > math.MaxInt64/priceMicro {
		return 0, ErrInvalidAmount
	}
	return int64(tokens) * priceMicro, nil
}
