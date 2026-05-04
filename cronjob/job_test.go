package cronjob

import (
	"testing"
	"time"
)

func TestKindString(t *testing.T) {
	cases := []struct {
		k    Kind
		want string
	}{
		{KindCron, "cron"},
		{KindInterval, "interval"},
		{KindOneshot, "oneshot"},
	}
	for _, c := range cases {
		if string(c.k) != c.want {
			t.Errorf("Kind=%q want %q", c.k, c.want)
		}
	}
}

func TestJobZero(t *testing.T) {
	j := Job{}
	if !j.LastFire.IsZero() {
		t.Error("zero Job should have zero LastFire")
	}
	if !j.NextFire.IsZero() {
		t.Error("zero Job should have zero NextFire")
	}
	if j.Disabled {
		t.Error("zero Job should not be disabled")
	}
	_ = time.Time{} // keep import
}
