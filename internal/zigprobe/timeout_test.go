package zigprobe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

func TestEffectiveTimeoutMSWithoutDeadline(t *testing.T) {
	got, err := effectiveTimeoutMS(context.Background(), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 5000 {
		t.Fatalf("want the configured timeout 5000, got %d", got)
	}
}

func TestEffectiveTimeoutMSClampsToNearerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	got, err := effectiveTimeoutMS(ctx, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if got == 0 || got > 300 {
		t.Fatalf("want a positive timeout no larger than the 300ms deadline, got %d", got)
	}
}

func TestEffectiveTimeoutMSKeepsFartherDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	got, err := effectiveTimeoutMS(ctx, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 5000 {
		t.Fatalf("a farther deadline must not raise the timeout: got %d", got)
	}
}

func TestEffectiveTimeoutMSNeverZero(t *testing.T) {
	// 0 means "library default" (2s) on the Zig side, so a sub-millisecond
	// budget must round up to 1ms rather than collapse to the default.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Microsecond)
	defer cancel()
	got, err := effectiveTimeoutMS(ctx, 5000)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want DeadlineExceeded, got %v", err)
		}
		return
	}
	if got != 1 {
		t.Fatalf("want a 1ms floor, got %d", got)
	}
}

func TestEffectiveTimeoutMSRefusesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := effectiveTimeoutMS(ctx, 5000); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestProbersRefuseCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := Prober{Iters: 1, TimeoutMS: 200}
	n := model.RNIC{ID: "rnic0", Device: "mlx5_0"}
	ep := model.Endpoint{ID: "mem0", Kind: model.EndpointMemory, NUMA: 0}
	if _, err := p.Loopback(ctx, n, ep, 1, 128*1024); !errors.Is(err, context.Canceled) {
		t.Fatalf("Loopback: want context.Canceled, got %v", err)
	}
	if _, err := p.Mutual(ctx, n, n); !errors.Is(err, context.Canceled) {
		t.Fatalf("Mutual: want context.Canceled, got %v", err)
	}
}

// The mutual payload reaches the C request as configured, and an unset
// Prober.MutualMsg sends the ToN'24 1-byte payload rather than a larger one.
func TestMutualReqCarriesMsgSize(t *testing.T) {
	src := model.RNIC{ID: "rnic0", Device: "dev0"}
	dst := model.RNIC{ID: "rnic1", Device: "dev1"}
	if got := mutualReqMsgSize(Prober{}, src, dst); got != 1 {
		t.Fatalf("default mutual msg_size = %d, want 1", got)
	}
	if got := mutualReqMsgSize(Prober{MutualMsg: 4096}, src, dst); got != 4096 {
		t.Fatalf("mutual msg_size = %d, want 4096", got)
	}
}
