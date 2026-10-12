package zigprobe

import (
	"errors"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

func TestPortErrNamesTheSkipReason(t *testing.T) {
	cases := []struct {
		msg  string
		want error
	}{
		{"Gid", model.ErrNoGID},
		{"NoLid: infiniband port 1 has lid 0", model.ErrNoLID},
		{"PortDown: infiniband port 1 not active", model.ErrPortNotActive},
		{"Port: link layers differ (RoCE and InfiniBand)", model.ErrProbe},
	}
	for _, c := range cases {
		err := portErr(c.msg)
		if !errors.Is(err, c.want) {
			t.Errorf("portErr(%q) = %v, want %v", c.msg, err, c.want)
		}
	}
	// The RoCE skip reads as it always did.
	if got := portErr("Gid").Error(); got != "no active rocev2 gid: Gid" {
		t.Errorf("RoCE skip reason = %q", got)
	}
	if got := portErr("NoLid: infiniband port 1 has lid 0").Error(); got != "infiniband port has no lid: NoLid: infiniband port 1 has lid 0" {
		t.Errorf("InfiniBand skip reason = %q", got)
	}
}
