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

func motorRequest() *openrtb_ext.RequestWrapper {
	return &openrtb_ext.RequestWrapper{BidRequest: &openrtb2.BidRequest{ID: "PRIVATE_REQUEST_ID", Imp: []openrtb2.Imp{{ID: "PRIVATE_IMP_ID", Ext: json.RawMessage(`{"prebid":{"storedrequest":{"id":"motortalk-2a3f7609"},"bidder":{"imprezia":{"siteId":"27d0f05d-3dc0-4958-8842-2564fe96932a","placementId":"motor-talk-main","request":"PRIVATE_CHAT","response":"PRIVATE_REPLY","timestamp":"PRIVATE_TIME","deviceContext":{},"userId":"PRIVATE_USER"}}}}`)}}}}
}

func TestCaptureBoundAndSanitization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	c, err := newCollector(path, time.Now().Add(time.Minute), 2)
	require.NoError(t, err)
	req := motorRequest()
	ctx, tr := c.begin(context.Background(), req)
	require.NotNil(t, tr)
	require.Same(t, tr, FromContext(ctx))
	tr.Input(req.BidRequest, true)
	tr.Event(Generated, OK, 1, 0)
	tr.Finish(&openrtb2.BidResponse{}, true)
	_, tr = c.begin(context.Background(), motorRequest())
	require.NotNil(t, tr)
	tr.Finish(nil, false)
	_, tr = c.begin(context.Background(), motorRequest())
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
	req := motorRequest()
	req.Test = 1
	_, tr := c.begin(context.Background(), req)
	require.Nil(t, tr)
	req.Test = 0
	req.Imp = append(req.Imp, req.Imp[0])
	_, tr = c.begin(context.Background(), req)
	require.Nil(t, tr)
	_, tr = c.begin(context.Background(), motorRequest())
	require.NotNil(t, tr)
	<-c.done
	_, other := c.begin(context.Background(), motorRequest())
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
			_, tr := c.begin(context.Background(), motorRequest())
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
	_, tr := c.begin(context.Background(), motorRequest())
	tr.Finish(nil, true)
	<-sink.started
	_, tr = c.begin(context.Background(), motorRequest())
	tr.Finish(nil, true)
	_, tr = c.begin(context.Background(), motorRequest())
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
	_, tr = c.begin(context.Background(), motorRequest())
	tr.Finish(nil, true)
	<-c.done
	require.Equal(t, 1, failed.calls, "JSON encoder retains the first write failure")
}

func TestUnrelatedInventoryAndDisabledCapture(t *testing.T) {
	ctx := context.Background()
	actual, tr := Begin(ctx, motorRequest())
	require.Equal(t, ctx, actual)
	require.Nil(t, tr)
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	c, err := newCollector(path, time.Now().Add(20*time.Millisecond), 1)
	require.NoError(t, err)
	req := motorRequest()
	req.Imp[0].Ext = json.RawMessage(`{"prebid":{"storedrequest":{"id":"knewz-4c4c401c"}}}`)
	_, tr = c.begin(ctx, req)
	require.Nil(t, tr)
	<-c.done
}
