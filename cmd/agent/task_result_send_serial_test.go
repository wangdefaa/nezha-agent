package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/nezhahq/agent/proto"
)

type concurrencyTrackingResultSink struct {
	mu          sync.Mutex
	inFlight    int32
	maxInFlight int32
	calls       int32
}

func (s *concurrencyTrackingResultSink) send(*pb.TaskResult) error {
	current := atomic.AddInt32(&s.inFlight, 1)
	defer atomic.AddInt32(&s.inFlight, -1)
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	if current > s.maxInFlight {
		s.maxInFlight = current
	}
	s.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	return nil
}

// gRPC Go ClientStream 禁止并发 SendMsg（https://pkg.go.dev/google.golang.org/grpc#ClientStream）。
// dispatchAgentTask 为每个 task 起 goroutine，各自在同一条 RequestTask 流上 send(result)；
// 面板下发的拨测任务经常重叠，所有 Send 必须经同一个串行器排队。
func TestSerialTaskResultSender_GuaranteesAtMostOneSendInFlight(t *testing.T) {
	sink := &concurrencyTrackingResultSink{}
	send := newSerialTaskResultSender(sink.send)

	var wg sync.WaitGroup
	const goroutines = 8
	const each = 4
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				if err := send(&pb.TaskResult{}); err != nil {
					t.Errorf("send returned unexpected error: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.maxInFlight != 1 {
		t.Fatalf("serial task-result sender must serialize Send; observed max-in-flight=%d, want 1", sink.maxInFlight)
	}
	if sink.calls != goroutines*each {
		t.Fatalf("every send must reach the sink; got %d, want %d", sink.calls, goroutines*each)
	}
}
