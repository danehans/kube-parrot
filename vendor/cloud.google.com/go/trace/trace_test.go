// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package trace

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/internal/testutil"
	"cloud.google.com/go/storage"
	"golang.org/x/net/context"
	api "google.golang.org/api/cloudtrace/v1"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	dspb "google.golang.org/genproto/googleapis/datastore/v1"
	"google.golang.org/grpc"
)

const testProjectID = "testproject"

type fakeRoundTripper struct {
	reqc chan *http.Request
}

func newFakeRoundTripper() *fakeRoundTripper {
	return &fakeRoundTripper{reqc: make(chan *http.Request)}
}

func (rt *fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.reqc <- r
	resp := &http.Response{
		Status:     "200 OK",
		StatusCode: 200,
		Body:       ioutil.NopCloser(strings.NewReader("{}")),
	}
	return resp, nil
}

func newTestClient(rt http.RoundTripper) *Client {
	t, err := NewClient(context.Background(), testProjectID, option.WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		panic(err)
	}
	return t
}

type fakeDatastoreServer struct {
	dspb.DatastoreServer
	fail bool
}

func (f *fakeDatastoreServer) Lookup(ctx context.Context, req *dspb.LookupRequest) (*dspb.LookupResponse, error) {
	if f.fail {
		return nil, errors.New("failed!")
	}
	return &dspb.LookupResponse{}, nil
}

// makeRequests makes some requests.
// req is an incoming request used to construct the trace.  traceClient is the
// client used to upload the trace.  rt is the trace client's http client's
// transport.  This is used to retrieve the trace uploaded by the client, if
// any.  If expectTrace is true, we expect a trace will be uploaded.  If
// synchronous is true, the call to Finish is expected not to return before the
// client has uploaded any traces.
func makeRequests(t *testing.T, req *http.Request, traceClient *Client, rt *fakeRoundTripper, synchronous bool, expectTrace bool) *http.Request {
	span := traceClient.SpanFromRequest(req)
	ctx := NewContext(context.Background(), span)

	// An HTTP request.
	{
		req2, err := http.NewRequest("GET", "http://example.com/bar", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp := &http.Response{StatusCode: 200}
		s := span.NewRemoteChild(req2)
		s.Finish(WithResponse(resp))
	}

	// An autogenerated API call.
	{
		rt := &fakeRoundTripper{reqc: make(chan *http.Request, 1)}
		hc := &http.Client{Transport: rt}
		computeClient, err := compute.New(hc)
		if err != nil {
			t.Fatal(err)
		}
		_, err = computeClient.Zones.List(testProjectID).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
	}

	// A cloud library call that uses the autogenerated API.
	{
		rt := &fakeRoundTripper{reqc: make(chan *http.Request, 1)}
		hc := &http.Client{Transport: rt}
		storageClient, err := storage.NewClient(context.Background(), option.WithHTTPClient(hc))
		if err != nil {
			t.Fatal(err)
		}
		var objAttrsList []*storage.ObjectAttrs
		it := storageClient.Bucket("testbucket").Objects(ctx, nil)
		for {
			objAttrs, err := it.Next()
			if err != nil && err != iterator.Done {
				t.Fatal(err)
			}
			if err == iterator.Done {
				break
			}
			objAttrsList = append(objAttrsList, objAttrs)
		}
	}

	// A cloud library call that uses grpc internally.
	for _, fail := range []bool{false, true} {
		srv, err := testutil.NewServer()
		if err != nil {
			t.Fatalf("creating test datastore server: %v", err)
		}
		dspb.RegisterDatastoreServer(srv.Gsrv, &fakeDatastoreServer{fail: fail})
		srv.Start()
		conn, err := grpc.Dial(srv.Addr, grpc.WithInsecure(), EnableGRPCTracingDialOption)
		if err != nil {
			t.Fatalf("connecting to test datastore server: %v", err)
		}
		datastoreClient, err := datastore.NewClient(ctx, testProjectID, option.WithGRPCConn(conn))
		if err != nil {
			t.Fatalf("creating datastore client: %v", err)
		}
		k := datastore.NewKey(ctx, "Entity", "stringID", 0, nil)
		e := new(datastore.Entity)
		datastoreClient.Get(ctx, k, e)
	}

	done := make(chan struct{})
	go func() {
		if synchronous {
			err := span.FinishWait()
			if err != nil {
				t.Errorf("Unexpected error from span.FinishWait: %v", err)
			}
		} else {
			span.Finish()
		}
		done <- struct{}{}
	}()
	if !expectTrace {
		<-done
		select {
		case <-rt.reqc:
			t.Errorf("Got a trace, expected none.")
		case <-time.After(5 * time.Millisecond):
		}
		return nil
	} else if !synchronous {
		<-done
		return <-rt.reqc
	} else {
		select {
		case <-done:
			t.Errorf("Synchronous Finish didn't wait for trace upload.")
			return <-rt.reqc
		case <-time.After(5 * time.Millisecond):
			r := <-rt.reqc
			<-done
			return r
		}
	}
}

func TestTrace(t *testing.T) {
	t.Parallel()
	testTrace(t, false)
}

func TestTraceWithWait(t *testing.T) {
	testTrace(t, true)
}

func testTrace(t *testing.T, synchronous bool) {
	req, err := http.NewRequest("GET", "http://example.com/foo", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header["X-Cloud-Trace-Context"] = []string{`0123456789ABCDEF0123456789ABCDEF/42;o=3`}

	rt := newFakeRoundTripper()
	traceClient := newTestClient(rt)

	uploaded := makeRequests(t, req, traceClient, rt, synchronous, true)

	if uploaded == nil {
		t.Fatalf("No trace uploaded, expected one.")
	}

	expected := api.Traces{
		Traces: []*api.Trace{
			{
				ProjectId: testProjectID,
				Spans: []*api.TraceSpan{
					{
						Kind: "RPC_CLIENT",
						Labels: map[string]string{
							"trace.cloud.google.com/http/host":        "example.com",
							"trace.cloud.google.com/http/method":      "GET",
							"trace.cloud.google.com/http/status_code": "200",
							"trace.cloud.google.com/http/url":         "http://example.com/bar",
						},
						Name: "/bar",
					},
					{
						Kind: "RPC_CLIENT",
						Labels: map[string]string{
							"trace.cloud.google.com/http/host":        "www.googleapis.com",
							"trace.cloud.google.com/http/method":      "GET",
							"trace.cloud.google.com/http/status_code": "200",
							"trace.cloud.google.com/http/url":         "https://www.googleapis.com/compute/v1/projects/testproject/zones",
						},
						Name: "/compute/v1/projects/testproject/zones",
					},
					{
						Kind: "RPC_CLIENT",
						Labels: map[string]string{
							"trace.cloud.google.com/http/host":        "www.googleapis.com",
							"trace.cloud.google.com/http/method":      "GET",
							"trace.cloud.google.com/http/status_code": "200",
							"trace.cloud.google.com/http/url":         "https://www.googleapis.com/storage/v1/b/testbucket/o",
						},
						Name: "/storage/v1/b/testbucket/o",
					},
					&api.TraceSpan{
						Kind:   "RPC_CLIENT",
						Labels: nil,
						Name:   "/google.datastore.v1.Datastore/Lookup",
					},
					&api.TraceSpan{
						Kind:   "RPC_CLIENT",
						Labels: map[string]string{"error": "rpc error: code = 2 desc = failed!"},
						Name:   "/google.datastore.v1.Datastore/Lookup",
					},
					{
						Kind: "RPC_SERVER",
						Labels: map[string]string{
							"trace.cloud.google.com/http/host":   "example.com",
							"trace.cloud.google.com/http/method": "GET",
							"trace.cloud.google.com/http/url":    "http://example.com/foo",
						},
						Name: "/foo",
					},
				},
				TraceId: "0123456789ABCDEF0123456789ABCDEF",
			},
		},
	}

	body, err := ioutil.ReadAll(uploaded.Body)
	if err != nil {
		t.Fatal(err)
	}
	var patch api.Traces
	err = json.Unmarshal(body, &patch)
	if err != nil {
		t.Fatal(err)
	}

	if len(patch.Traces) != len(expected.Traces) || len(patch.Traces[0].Spans) != len(expected.Traces[0].Spans) {
		got, _ := json.Marshal(patch)
		want, _ := json.Marshal(expected)
		t.Fatalf("PatchTraces request: got %s want %s", got, want)
	}

	n := len(patch.Traces[0].Spans)
	rootSpan := patch.Traces[0].Spans[n-1]
	for i, s := range patch.Traces[0].Spans {
		if a, b := s.StartTime, s.EndTime; a > b {
			t.Errorf("span %d start time is later than its end time (%q, %q)", i, a, b)
		}
		if a, b := rootSpan.StartTime, s.StartTime; a > b {
			t.Errorf("trace start time is later than span %d start time (%q, %q)", i, a, b)
		}
		if a, b := s.EndTime, rootSpan.EndTime; a > b {
			t.Errorf("span %d end time is later than trace end time (%q, %q)", i, a, b)
		}
		if i > 1 && i < n-1 {
			if a, b := patch.Traces[0].Spans[i-1].EndTime, s.StartTime; a > b {
				t.Errorf("span %d end time is later than span %d start time (%q, %q)", i-1, i, a, b)
			}
		}
	}

	if x := rootSpan.ParentSpanId; x != 42 {
		t.Errorf("Incorrect ParentSpanId: got %d want %d", x, 42)
	}
	for i, s := range patch.Traces[0].Spans {
		if x, y := rootSpan.SpanId, s.ParentSpanId; i < n-1 && x != y {
			t.Errorf("Incorrect ParentSpanId in span %d: got %d want %d", i, y, x)
		}
	}
	for i, s := range patch.Traces[0].Spans {
		s.EndTime = ""
		labels := &expected.Traces[0].Spans[i].Labels
		for key, value := range *labels {
			if v, ok := s.Labels[key]; !ok {
				t.Errorf("Span %d is missing Label %q:%q", i, key, value)
			} else if key == "trace.cloud.google.com/http/url" {
				if !strings.HasPrefix(v, value) {
					t.Errorf("Span %d Label %q: got value %q want prefix %q", i, key, v, value)
				}
			} else if v != value {
				t.Errorf("Span %d Label %q: got value %q want %q", i, key, v, value)
			}
		}
		for key := range s.Labels {
			if _, ok := (*labels)[key]; key != "trace.cloud.google.com/stacktrace" && !ok {
				t.Errorf("Span %d: unexpected label %q", i, key)
			}
		}
		*labels = nil
		s.Labels = nil
		s.ParentSpanId = 0
		if s.SpanId == 0 {
			t.Errorf("Incorrect SpanId: got 0 want nonzero")
		}
		s.SpanId = 0
		s.StartTime = ""
	}
	if !reflect.DeepEqual(patch, expected) {
		got, _ := json.Marshal(patch)
		want, _ := json.Marshal(expected)
		t.Errorf("PatchTraces request: got %s want %s", got, want)
	}
}

func TestNoTrace(t *testing.T) {
	testNoTrace(t, false)
}

func TestNoTraceWithWait(t *testing.T) {
	testNoTrace(t, true)
}

func testNoTrace(t *testing.T, synchronous bool) {
	for _, header := range []string{
		`0123456789ABCDEF0123456789ABCDEF/42;o=2`,
		`0123456789ABCDEF0123456789ABCDEF/42;o=0`,
		`0123456789ABCDEF0123456789ABCDEF/42`,
		`0123456789ABCDEF0123456789ABCDEF`,
		``,
	} {
		req, err := http.NewRequest("GET", "http://example.com/foo", nil)
		if header != "" {
			req.Header["X-Cloud-Trace-Context"] = []string{header}
		}
		if err != nil {
			t.Fatal(err)
		}
		rt := newFakeRoundTripper()
		traceClient := newTestClient(rt)
		uploaded := makeRequests(t, req, traceClient, rt, synchronous, false)
		if uploaded != nil {
			t.Errorf("Got a trace, expected none.")
		}
	}
}

func TestSample(t *testing.T) {
	// A deterministic test of the sampler logic.
	type testCase struct {
		rate   float64
		maxqps float64
		want   int
	}
	const delta = 25 * time.Millisecond
	for _, test := range []testCase{
		// qps won't matter, so we will sample half of the 79 calls
		{0.50, 100, 40},
		// with 1 qps and a burst of 2, we will sample twice in second #1, once in the partial second #2
		{0.50, 1, 3},
	} {
		sp, err := NewLimitedSampler(test.rate, test.maxqps)
		if err != nil {
			t.Fatal(err)
		}
		s := sp.(*sampler)
		sampled := 0
		tm := time.Now()
		for i := 0; i < 80; i++ {
			if ok, _, _ := s.sample(tm, float64(i%2)); ok {
				sampled++
			}
			tm = tm.Add(delta)
		}
		if sampled != test.want {
			t.Errorf("rate=%f, maxqps=%f: got %d samples, want %d", test.rate, test.maxqps, sampled, test.want)
		}
	}
}

func TestSampling(t *testing.T) {
	t.Parallel()
	// This scope tests sampling in a larger context, with real time and randomness.
	wg := sync.WaitGroup{}
	type testCase struct {
		rate          float64
		maxqps        float64
		expectedRange [2]int
	}
	for _, test := range []testCase{
		{0, 5, [2]int{0, 0}},
		{5, 0, [2]int{0, 0}},
		{0.50, 100, [2]int{20, 60}},
		{0.50, 1, [2]int{3, 4}}, // Windows, with its less precise clock, sometimes gives 4.
	} {
		wg.Add(1)
		go func(test testCase) {
			rt := newFakeRoundTripper()
			traceClient := newTestClient(rt)
			traceClient.bundler.BundleByteLimit = 1
			p, err := NewLimitedSampler(test.rate, test.maxqps)
			if err != nil {
				t.Fatalf("NewLimitedSampler: %v", err)
			}
			traceClient.SetSamplingPolicy(p)
			ticker := time.NewTicker(25 * time.Millisecond)
			sampled := 0
			for i := 0; i < 79; i++ {
				req, err := http.NewRequest("GET", "http://example.com/foo", nil)
				if err != nil {
					t.Fatal(err)
				}
				span := traceClient.SpanFromRequest(req)
				span.Finish()
				select {
				case <-rt.reqc:
					<-ticker.C
					sampled++
				case <-ticker.C:
				}
			}
			ticker.Stop()
			if test.expectedRange[0] > sampled || sampled > test.expectedRange[1] {
				t.Errorf("rate=%f, maxqps=%f: got %d samples want ∈ %v", test.rate, test.maxqps, sampled, test.expectedRange)
			}
			wg.Done()
		}(test)
	}
	wg.Wait()
}

func TestBundling(t *testing.T) {
	t.Parallel()
	rt := newFakeRoundTripper()
	traceClient := newTestClient(rt)
	traceClient.bundler.DelayThreshold = time.Second / 2
	traceClient.bundler.BundleCountThreshold = 10
	p, err := NewLimitedSampler(1, 99) // sample every request.
	if err != nil {
		t.Fatalf("NewLimitedSampler: %v", err)
	}
	traceClient.SetSamplingPolicy(p)

	for i := 0; i < 35; i++ {
		go func() {
			req, err := http.NewRequest("GET", "http://example.com/foo", nil)
			if err != nil {
				t.Fatal(err)
			}
			span := traceClient.SpanFromRequest(req)
			span.Finish()
		}()
	}

	// Read the first three bundles.
	<-rt.reqc
	<-rt.reqc
	<-rt.reqc

	// Test that the fourth bundle isn't sent early.
	select {
	case <-rt.reqc:
		t.Errorf("bundle sent too early")
	case <-time.After(time.Second / 4):
		<-rt.reqc
	}

	// Test that there aren't extra bundles.
	select {
	case <-rt.reqc:
		t.Errorf("too many bundles sent")
	case <-time.After(time.Second):
	}
}