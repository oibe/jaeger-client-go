// Copyright (c) 2015 Uber Technologies, Inc.

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package jaeger

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uber/jaeger-client-go/testutils"
	z "github.com/uber/jaeger-client-go/thrift/gen/zipkincore"

	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/uber/jaeger-client-go/transport"
	"github.com/uber/jaeger-client-go/transport/tchannel"
	"github.com/uber/jaeger-client-go/transport/udp"
	"github.com/uber/tchannel-go/thrift"
)

type reporterSuite struct {
	suite.Suite
	tracer      opentracing.Tracer
	closer      io.Closer
	serviceName string
	reporter    *remoteReporter
	collector   *fakeZipkinCollector
	stats       *InMemoryStatsCollector
}

func (s *reporterSuite) SetupTest() {
	s.stats = NewInMemoryStatsCollector()
	metrics := NewMetrics(s.stats, nil)
	s.serviceName = "DOOP"
	s.collector = &fakeZipkinCollector{}
	sender := &tchannel.Transport{Client: s.collector}
	s.reporter = NewRemoteReporter(sender, &ReporterOptions{Metrics: metrics}).(*remoteReporter)

	s.tracer, s.closer = NewTracer(
		"reporter-test-service",
		NewConstSampler(true),
		s.reporter,
		TracerOptions.Metrics(metrics))
	s.NotNil(s.tracer)
}

func (s *reporterSuite) TearDownTest() {
	s.closer.Close()
	s.tracer = nil
	s.reporter = nil
	s.collector = nil
}

func TestReporter(t *testing.T) {
	suite.Run(t, new(reporterSuite))
}

func (s *reporterSuite) flushReporter() {
	// Wait for reporter queue to add spans to buffer. We could've called reporter.Close(),
	// but then it fails when the test suite calls close on it again (via tracer's Closer).
	time.Sleep(5 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(1)
	s.reporter.flushSignal <- &wg
	wg.Wait()
}

func (s *reporterSuite) TestRootSpanAnnotations() {
	sp := s.tracer.StartSpan("get_name")
	ext.SpanKind.Set(sp, ext.SpanKindRPCServer)
	ext.PeerService.Set(sp, s.serviceName)
	sp.Finish()
	s.flushReporter()
	s.Equal(1, len(s.collector.Spans()))
	zSpan := s.collector.Spans()[0]
	s.NotNil(findAnnotation(zSpan, "sr"), "expecting sr annotation")
	s.NotNil(findAnnotation(zSpan, "ss"), "expecting ss annotation")
	s.NotNil(findBinaryAnnotation(zSpan, "ca"), "expecting ca annotation")
	s.NotNil(findBinaryAnnotation(zSpan, JaegerClientTag), "expecting client version tag")
	s.EqualValues(1, s.stats.GetCounterValues()["jaeger.reporter-spans|state=success"])
}

func (s *reporterSuite) TestClientSpanAnnotations() {
	sp := s.tracer.StartSpan("get_name")
	ext.SpanKind.Set(sp, ext.SpanKindRPCServer)
	ext.PeerService.Set(sp, s.serviceName)
	sp2 := opentracing.StartChildSpan(sp, "get_last_name")
	ext.SpanKind.Set(sp2, ext.SpanKindRPCClient)
	ext.PeerService.Set(sp2, s.serviceName)
	sp2.Finish()
	sp.Finish()
	s.flushReporter()
	s.Equal(2, len(s.collector.Spans()))
	zSpan := s.collector.Spans()[0] // child span is reported first
	s.EqualValues(zSpan.ID, sp2.(*span).spanID)
	s.Equal(2, len(zSpan.Annotations), "expecting two annotations, cs and cr")
	s.Equal(1, len(zSpan.BinaryAnnotations), "expecting one binary annotation sa")
	s.NotNil(findAnnotation(zSpan, "cs"), "expecting cs annotation")
	s.NotNil(findAnnotation(zSpan, "cr"), "expecting cr annotation")
	s.NotNil(findBinaryAnnotation(zSpan, "sa"), "expecting sa annotation")
	s.EqualValues(2, s.stats.GetCounterValues()["jaeger.reporter-spans|state=success"])
}

func (s *reporterSuite) TestTagsAndEvents() {
	sp := s.tracer.StartSpan("get_name")
	sp.LogEvent("hello")
	sp.LogEvent(strings.Repeat("long event", 30))
	expected := []string{"long", "ping", "awake", "awake", "one", "two", "three", "bite me",
		"jaegerClient", "lc", "does not compute"}
	sp.SetTag("long", strings.Repeat("x", 300))
	sp.SetTag("ping", "pong")
	sp.SetTag("awake", true)
	sp.SetTag("awake", false)
	sp.SetTag("one", 1)
	sp.SetTag("two", int32(2))
	sp.SetTag("three", int64(3))
	sp.SetTag("bite me", []byte{1})
	sp.SetTag("does not compute", sp) // should be converted to string
	sp.Finish()
	s.flushReporter()
	s.Equal(1, len(s.collector.Spans()))
	zSpan := s.collector.Spans()[0]
	s.Equal(2, len(zSpan.Annotations), "expecting two annotations for events")
	s.Equal(len(expected), len(zSpan.BinaryAnnotations),
		"expecting %d binary annotations", len(expected))
	binAnnos := []string{}
	for _, a := range zSpan.BinaryAnnotations {
		binAnnos = append(binAnnos, string(a.Key))
	}
	sort.Strings(expected)
	sort.Strings(binAnnos)
	s.Equal(expected, binAnnos, "expecting %d binary annotations", len(expected))

	s.NotNil(findAnnotation(zSpan, "hello"), "expecting 'hello' annotation")

	longEvent := false
	for _, a := range zSpan.Annotations {
		if strings.HasPrefix(a.Value, "long event") {
			longEvent = true
			s.EqualValues(256, len(a.Value))
		}
	}
	s.True(longEvent, "Must have truncated and saved long event name")

	for i := range expected {
		s.NotNil(findBinaryAnnotation(zSpan, expected[i]), "expecting annotation '%s'", expected[i])
	}
	doesNotCompute := findBinaryAnnotation(zSpan, "does not compute")
	s.NotNil(doesNotCompute)
	doesNotComputeStr := fmt.Sprintf("%+v", sp)
	s.Equal(doesNotComputeStr, string(doesNotCompute.Value))

	longStr := findBinaryAnnotation(zSpan, "long")
	s.EqualValues(256, len(longStr.Value), "long tag valur must be truncated")
}

func TestTCollectorReporter(t *testing.T) {
	collector, err := testutils.StartMockTCollector()
	require.NoError(t, err)
	defer collector.Close()

	testRemoteReporter(t,
		func(m *Metrics) (transport.Transport, error) {
			return tchannel.New(collector.Channel, "", 0), nil
		},
		func() []*z.Span {
			return collector.GetZipkinSpans()
		})
}

func TestUDPReporter(t *testing.T) {
	agent, err := testutils.StartMockAgent()
	require.NoError(t, err)
	defer agent.Close()

	testRemoteReporter(t,
		func(m *Metrics) (transport.Transport, error) {
			return udp.NewUDPTransport(agent.SpanServerAddr(), 0)
		},
		func() []*z.Span {
			return agent.GetZipkinSpans()
		})
}

func testRemoteReporter(
	t *testing.T,
	factory func(m *Metrics) (transport.Transport, error),
	getSpans func() []*z.Span,
) {
	stats := NewInMemoryStatsCollector()
	metrics := NewMetrics(stats, nil)

	sender, err := factory(metrics)
	require.NoError(t, err)
	reporter := NewRemoteReporter(sender, &ReporterOptions{Metrics: metrics}).(*remoteReporter)

	tracer, closer := NewTracer(
		"reporter-test-service",
		NewConstSampler(true),
		reporter,
		TracerOptions.Metrics(metrics))

	span := tracer.StartSpan("leela")
	ext.SpanKind.Set(span, ext.SpanKindRPCClient)
	ext.PeerService.Set(span, "downstream")
	span.Finish()
	closer.Close() // close the tracer, which also closes and flushes the reporter
	// however, in case of UDP reporter it's fire and forget, so we need to wait a bit
	time.Sleep(5 * time.Millisecond)

	spans := getSpans()
	require.Equal(t, 1, len(spans))
	assert.Equal(t, "leela", spans[0].Name)
	sa := findBinaryAnnotation(spans[0], z.SERVER_ADDR)
	require.NotNil(t, sa)
	assert.Equal(t, "downstream", sa.Host.ServiceName)

	assert.EqualValues(t, 1, stats.GetCounterValues()["jaeger.reporter-spans|state=success"], "success metric")
	assert.EqualValues(t, 0, stats.GetCounterValues()["jaeger.reporter-spans|state=failure"], "failure metric")
}

func (s *reporterSuite) TestMemoryReporterReport() {
	sp := s.tracer.StartSpan("leela")
	ext.PeerService.Set(sp, s.serviceName)
	reporter := NewInMemoryReporter()
	reporter.Report(sp.(*span))
	s.Equal(1, reporter.SpansSubmitted(), "expected number of spans submitted")
	reporter.Close()
}

func findAnnotation(span *z.Span, name string) *z.Annotation {
	for _, a := range span.Annotations {
		if a.Value == name {
			return a
		}
	}
	return nil
}

func findBinaryAnnotation(span *z.Span, name string) *z.BinaryAnnotation {
	for _, a := range span.BinaryAnnotations {
		if a.Key == name {
			return a
		}
	}
	return nil
}

type fakeZipkinCollector struct {
	spans []*z.Span
	mutex sync.Mutex
}

func (c *fakeZipkinCollector) SubmitZipkinBatch(ctx thrift.Context, spans []*z.Span) ([]*z.Response, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.spans = append(c.spans, spans...)
	return nil, nil
}

func (c *fakeZipkinCollector) Spans() []*z.Span {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	res := make([]*z.Span, len(c.spans))
	copy(res, c.spans)
	return res
}
