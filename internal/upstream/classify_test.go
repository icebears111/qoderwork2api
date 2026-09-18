package upstream

import (
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"error":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"isQuotaExceeded":true}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		{401, `TOKEN_EXPIRE`, ErrTokenExpired},
		{401, `bad pat`, ErrClient},
		{500, `boom`, ErrServer},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestErrorIs(t *testing.T) {
	err := &Error{Kind: ErrHardCredit, Status: 402, Msg: "x"}
	if !errors.Is(err, ErrHardCredit) {
		t.Error("errors.Is failed")
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrHardCredit {
		t.Error("errors.As failed")
	}
}
