package streamwatch

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestResponseLimitRejectsOverflowWithoutSilentTruncation(t *testing.T) {
	ctx := WithResponseLimit(context.Background(), 4)
	for _, tc := range []struct {
		input    string
		overflow bool
	}{{"abcd", false}, {"abcde", true}} {
		data, err := io.ReadAll(LimitReader(ctx, strings.NewReader(tc.input)))
		if tc.overflow {
			if !errors.Is(err, ErrResponseTooLarge) {
				t.Fatal("overflow silently truncated", err)
			}
		} else if err != nil || string(data) != tc.input {
			t.Fatal("exact limit rejected", err)
		}
	}
}
