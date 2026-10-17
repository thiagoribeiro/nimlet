package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestValidators(t *testing.T) {
	ok := []error{
		ValidateUsername("alice"), ValidateUsername("joão.silva-1"), ValidateUsername(strings.Repeat("é", MaxUsernameLen)),
		ValidatePassword("x"), ValidatePassword(strings.Repeat("p", MaxPasswordBytes)),
		ValidateText("name", "", 10), ValidateText("name", "Sala Geral", 10),
		ValidateToken("txn", "abc", 3),
	}
	for i, err := range ok {
		if err != nil {
			t.Errorf("case %d: unexpected %v", i, err)
		}
	}
	bad := []error{
		ValidateUsername(""), ValidateUsername("a b"), ValidateUsername("tab\t"), ValidateUsername(strings.Repeat("a", MaxUsernameLen+1)),
		ValidatePassword(""), ValidatePassword(strings.Repeat("p", MaxPasswordBytes+1)),
		ValidateText("name", strings.Repeat("a", 11), 10), ValidateText("name", "\xff", 10),
		ValidateToken("txn", "", 3), ValidateToken("txn", "abcd", 3),
	}
	for i, err := range bad {
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("case %d: want ErrInvalidArgument, got %v", i, err)
		}
	}
}
