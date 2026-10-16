package hol100diag

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/require"
)

func learnrithmRequest() *openrtb_ext.RequestWrapper {
	return &openrtb_ext.RequestWrapper{BidRequest: &openrtb2.BidRequest{ID: "PRIVATE_REQUEST_ID", Imp: []openrtb2.Imp{{ID: "PRIVATE_IMP_ID", Ext: json.RawMessage(`{"prebid":{"storedrequest":{"id":"learnrithm-d122e2e9"},"bidder":{"imprezia":{"siteId":"7ce7e8f5-32cb-4788-92bf-cc2a04d75ec3","placementId":"learnrithm-lesson-main","request":"PRIVATE_CHAT","response":"PRIVATE_REPLY","timestamp":"PRIVATE_TIME","deviceContext":{},"userId":"PRIVATE_USER"}}}}`)}}}}
}

func TestCaptureBoundAndSanitization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	c, err := newCollector(path, time.Now().Add(time.Minute), 2)
	require.NoError(t, err)
	req := learnrithmRequest()
	ctx, tr := c.begin(context.Background(), req)
	require.NotNil(t, tr)
	require.Same(t, tr, FromContext(ctx))
	tr.Input(req.BidRequest, true)
	tr.Event(Generated, OK, 1, 0)
	tr.Finish(&openrtb2.BidResponse{}, true)
	_, tr = c.begin(context.Background(), learnrithmRequest())
	require.NotNil(t, tr)
	tr.Finish(nil, false)
	_, tr = c.begin(context.Background(), learnrithmRequest())
	require.Nil(t, tr)
	<-c.done
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "PRIVATE_")
	require.Contains(t, string(raw), `"admitted":2`)
	require.Contains(t, string(raw), `"written":2`)
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), st.Mode().Perm())
}

func TestScopeExpiryAndExclusiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	c, err := newCollector(path, time.Now().Add(30*time.Millisecond), 10)
	require.NoError(t, err)
	_, err = newCollector(path, time.Now().Add(time.Minute), 10)
	require.Error(t, err)
	req := learnrithmRequest()
	req.Test = 1
	_, tr := c.begin(context.Background(), req)
	require.Nil(t, tr)
	req.Test = 0
	req.Imp = append(req.Imp, req.Imp[0])
	_, tr = c.begin(context.Background(), req)
	require.Nil(t, tr)
	_, tr = c.begin(context.Background(), learnrithmRequest())
	require.NotNil(t, tr)
	<-c.done
	_, other := c.begin(context.Background(), learnrithmRequest())
	require.Nil(t, other)
	tr.Finish(nil, false)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"unfinished":1`)
	_, err = newCollector(filepath.Join(t.TempDir(), "bad"), time.Now().Add(16*time.Minute), 1000)
	require.Error(t, err)
}

func TestConcurrentCaptureAndEventBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	c, err := newCollector(path, time.Now().Add(time.Minute), 100)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, tr := c.begin(context.Background(), learnrithmRequest())
			require.NotNil(t, tr)
			for j := 0; j < 100; j++ {
				tr.Event(HTTPExecute, OK, 0, 0)
			}
			tr.Finish(nil, false)
		}()
	}
	wg.Wait()
	<-c.done
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, line := range lines {
		var r Record
		require.NoError(t, json.Unmarshal([]byte(line), &r))
		if r.Kind == "auction" {
			require.LessOrEqual(t, len(r.Events), 32)
			require.Equal(t, 68, r.EventsDropped)
		}
	}
	require.Contains(t, string(raw), `"admitted":100`)
}

type blockedWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}
func (w *blockedWriter) Close() error { return nil }

type failedWriter struct{ calls int }

func (w *failedWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("PRIVATE_DISK_ERROR")
}
func (w *failedWriter) Close() error { return nil }

func TestWriterFailureAndFullQueueDoNotBlockAuctions(t *testing.T) {
	c := &Collector{until: time.Now().Add(time.Minute), limit: 3, queue: make(chan Record, 1), stop: make(chan struct{}, 1), done: make(chan struct{})}
	sink := &blockedWriter{started: make(chan struct{}), release: make(chan struct{})}
	go c.write(sink, time.Minute)
	_, tr := c.begin(context.Background(), learnrithmRequest())
	tr.Finish(nil, true)
	<-sink.started
	_, tr = c.begin(context.Background(), learnrithmRequest())
	tr.Finish(nil, true)
	_, tr = c.begin(context.Background(), learnrithmRequest())
	returned := make(chan struct{})
	go func() { tr.Finish(nil, true); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("full diagnostic queue blocked auction")
	}
	require.Equal(t, uint64(1), c.dropped.Load())
	close(sink.release)
	<-c.done
	c = &Collector{until: time.Now().Add(time.Minute), limit: 1, queue: make(chan Record, 1), stop: make(chan struct{}, 1), done: make(chan struct{})}
	failed := &failedWriter{}
	go c.write(failed, time.Minute)
	_, tr = c.begin(context.Background(), learnrithmRequest())
	tr.Finish(nil, true)
	<-c.done
	require.Equal(t, 1, failed.calls, "JSON encoder retains the first write failure")
}

func TestUnrelatedInventoryAndDisabledCapture(t *testing.T) {
	ctx := context.Background()
	actual, tr := Begin(ctx, learnrithmRequest())
	require.Equal(t, ctx, actual)
	require.Nil(t, tr)
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	c, err := newCollector(path, time.Now().Add(20*time.Millisecond), 1)
	require.NoError(t, err)
	req := learnrithmRequest()
	req.Imp[0].Ext = json.RawMessage(`{"prebid":{"storedrequest":{"id":"knewz-4c4c401c"}}}`)
	_, tr = c.begin(ctx, req)
	require.Nil(t, tr)
	<-c.done
}

func TestHOL111PublisherScope(t *testing.T) {
	for _, tc := range []struct {
		id, surface, slot string
		admitted          bool
	}{
		{"learnrithm-d122e2e9", "7ce7e8f5-32cb-4788-92bf-cc2a04d75ec3", "learnrithm-lesson-main", true},
		{"promptessor-bfc732a4", "d4f698a8-507a-4622-9c33-da53a198650d", "promptessor-main", true},
		{"motortalk-2a3f7609", "27d0f05d-3dc0-4958-8842-2564fe96932a", "motor-talk-main", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			c, err := newCollector(filepath.Join(t.TempDir(), "scope.jsonl"), time.Now().Add(30*time.Millisecond), 1)
			require.NoError(t, err)
			ext, err := json.Marshal(map[string]any{"prebid": map[string]any{
				"storedrequest": map[string]string{"id": tc.id},
				"bidder":        map[string]any{"imprezia": map[string]any{"siteId": tc.surface, "placementId": tc.slot, "request": "PRIVATE_CHAT", "response": "PRIVATE_REPLY", "timestamp": "PRIVATE_TIME", "deviceContext": map[string]any{}}},
			}})
			require.NoError(t, err)
			req := &openrtb_ext.RequestWrapper{BidRequest: &openrtb2.BidRequest{Imp: []openrtb2.Imp{{ID: "PRIVATE_IMP", Ext: ext}}}}
			_, tr := c.begin(context.Background(), req)
			if tc.admitted {
				require.NotNil(t, tr)
				require.True(t, tr.record.Merged.SurfaceMatches)
				require.True(t, tr.record.Merged.SlotMatches)
				tr.Input(req.BidRequest, false)
				require.True(t, tr.record.Mapped.SurfaceMatches)
				raw, err := json.Marshal(tr.record)
				require.NoError(t, err)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(raw, &fields))
				require.Equal(t, tc.id, fields["placement"])
				require.NotContains(t, string(raw), "PRIVATE_")
				tr.Finish(nil, false)
			} else {
				require.Nil(t, tr)
			}
			<-c.done
		})
	}
}

func TestHOL111RequestAliasesExcluded(t *testing.T) {
	c, err := newCollector(filepath.Join(t.TempDir(), "aliases.jsonl"), time.Now().Add(30*time.Millisecond), 1)
	require.NoError(t, err)
	req := learnrithmRequest()
	req.Ext = json.RawMessage(`{"prebid":{"aliases":{"alternate":"imprezia"}}}`)
	_, tr := c.begin(context.Background(), req)
	require.Nil(t, tr, "alias adapter events cannot be compared to direct-seat final bids")
	require.Zero(t, c.admitted.Load())
	<-c.done
}

func TestHOL111NativeWithStubCompanions(t *testing.T) {
	c, err := newCollector(filepath.Join(t.TempDir(), "multi.jsonl"), time.Now().Add(time.Minute), 2)
	require.NoError(t, err)
	req := learnrithmRequest()
	native := req.Imp[0]
	req.Imp = []openrtb2.Imp{{ID: "banner", Ext: json.RawMessage(`{"prebid":{"bidder":{"tpc":{}}}}`)}, native, {ID: "video", Ext: json.RawMessage(`{"prebid":{"bidder":{"tpc":{}}}}`)}}
	_, tr := c.begin(context.Background(), req)
	require.NotNil(t, tr, "the live three-format Learnrithm auction must be observable")
	tr.Input(req.BidRequest, false)
	require.True(t, tr.record.Mapped.Request)
	tr.Finish(&openrtb2.BidResponse{SeatBid: []openrtb2.SeatBid{{Seat: "imprezia", Bid: []openrtb2.Bid{{ImpID: native.ID}, {ImpID: "banner"}}}}}, true)
	req.Imp[0].Ext = json.RawMessage(`{"prebid":{"bidder":{"imprezia":{}}}}`)
	_, other := c.begin(context.Background(), req)
	require.Nil(t, other, "exclude another Imprezia impression to avoid attributing batched HTTP results")
	_, tr = c.begin(context.Background(), learnrithmRequest())
	require.NotNil(t, tr)
	tr.Finish(nil, false)
	<-c.done

}
