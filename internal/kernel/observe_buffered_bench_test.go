package kernel

import (
	"sort"
	"testing"
	"time"
)

// BenchmarkAppend_SlowBufferedObserver measures what the run pays to append
// while one buffered observer sleeps 100ms per record, under each policy. It
// reports the append p99 and fails if it is not bounded: a blocking observer
// would put the 100ms sleep straight into the p99.
//
// The full 10k-record / 100ms acceptance run is
// TestObserveBuffered_AppendP99BoundedWithSlowConsumer; this one scales with
// b.N and stays short.
func BenchmarkAppend_SlowBufferedObserver(b *testing.B) {
	for _, policy := range []Policy{DropOldest, DropNewest, SignalGap} {
		b.Run(policy.String(), func(b *testing.B) {
			k := newKernel("bench", nil)
			obs, err := k.ObserveBuffered(func(Record) { time.Sleep(100 * time.Millisecond) }, 16, policy,
				gapOpts(policy, func(Gap) {})...)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { obs.Cancel(); <-obs.Done() }()

			body := frameBody("f")
			lat := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				start := time.Now()
				k.Frame(body, RecordAt{})
				lat = append(lat, time.Since(start))
			}
			b.StopTimer()

			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			p99 := lat[len(lat)*99/100]
			b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns/append")
			if p99 > 20*time.Millisecond {
				b.Fatalf("append p99 = %v with a 100ms consumer, want < 20ms", p99)
			}
		})
	}
}
