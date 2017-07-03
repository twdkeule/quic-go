package h2quic

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	quic "github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type mockStream struct {
	id           protocol.StreamID
	dataToRead   bytes.Buffer
	dataWritten  bytes.Buffer
	reset        bool
	closed       bool
	remoteClosed bool

	unblockRead chan struct{}
	ctx         context.Context
	ctxCancel   context.CancelFunc
}

var _ quic.Stream = &mockStream{}

func newMockStream(id protocol.StreamID) *mockStream {
	s := &mockStream{
		id:          id,
		unblockRead: make(chan struct{}),
	}
	s.ctx, s.ctxCancel = context.WithCancel(context.Background())
	return s
}

func (s *mockStream) Close() error                          { s.closed = true; s.ctxCancel(); return nil }
func (s *mockStream) CancelRead(quic.ErrorCode) error       { s.reset = true; return nil }
func (s *mockStream) CancelWrite(quic.ErrorCode) error      { panic("not implemented") }
func (s *mockStream) CloseRemote(offset protocol.ByteCount) { s.remoteClosed = true; s.ctxCancel() }
func (s mockStream) StreamID() protocol.StreamID            { return s.id }
func (s *mockStream) Context() context.Context              { return s.ctx }
func (s *mockStream) SetDeadline(time.Time) error           { panic("not implemented") }
func (s *mockStream) SetReadDeadline(time.Time) error       { panic("not implemented") }
func (s *mockStream) SetWriteDeadline(time.Time) error      { panic("not implemented") }

func (s *mockStream) Read(p []byte) (int, error) {
	n, _ := s.dataToRead.Read(p)
	if n == 0 { // block if there's no data
		<-s.unblockRead
		return 0, io.EOF
	}
	return n, nil // never return an EOF
}
func (s *mockStream) Write(p []byte) (int, error) { return s.dataWritten.Write(p) }

var _ = Describe("Response Writer", func() {
	var (
		w            *responseWriter
		headerStream *mockStream
		dataStream   *mockStream
		session      *mockSession
	)
	pushTarget := "/push_example"

	BeforeEach(func() {
		headerStream = &mockStream{}
		headerStream.id = protocol.StreamID(3)
		dataStream = &mockStream{}
		dataStream.id = protocol.StreamID(5)
		session = &mockSession{}
		handlerFunc := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
		w = newResponseWriter(headerStream, &sync.Mutex{}, dataStream, dataStream.id, newSessionSettings(), session, handlerFunc)
	})

	decodeHeaderFields := func() map[string][]string {
		fields := make(map[string][]string)
		decoder := hpack.NewDecoder(4096, func(hf hpack.HeaderField) {})
		h2framer := http2.NewFramer(nil, bytes.NewReader(headerStream.dataWritten.Bytes()))

		frame, err := h2framer.ReadFrame()
		Expect(err).ToNot(HaveOccurred())
		Expect(frame).To(BeAssignableToTypeOf(&http2.HeadersFrame{}))
		hframe := frame.(*http2.HeadersFrame)
		mhframe := &http2.MetaHeadersFrame{HeadersFrame: hframe}
		Expect(mhframe.StreamID).To(BeEquivalentTo(dataStream.id))
		mhframe.Fields, err = decoder.DecodeFull(hframe.HeaderBlockFragment())
		Expect(err).ToNot(HaveOccurred())
		for _, p := range mhframe.Fields {
			fields[p.Name] = append(fields[p.Name], p.Value)
		}
		return fields
	}

	It("writes status", func() {
		w.WriteHeader(http.StatusTeapot)
		fields := decodeHeaderFields()
		Expect(fields).To(HaveLen(1))
		Expect(fields).To(HaveKeyWithValue(":status", []string{"418"}))
	})

	It("writes headers", func() {
		w.Header().Add("content-length", "42")
		w.WriteHeader(http.StatusTeapot)
		fields := decodeHeaderFields()
		Expect(fields).To(HaveKeyWithValue("content-length", []string{"42"}))
	})

	It("writes multiple headers with the same name", func() {
		const cookie1 = "test1=1; Max-Age=7200; path=/"
		const cookie2 = "test2=2; Max-Age=7200; path=/"
		w.Header().Add("set-cookie", cookie1)
		w.Header().Add("set-cookie", cookie2)
		w.WriteHeader(http.StatusTeapot)
		fields := decodeHeaderFields()
		Expect(fields).To(HaveKey("set-cookie"))
		cookies := fields["set-cookie"]
		Expect(cookies).To(ContainElement(cookie1))
		Expect(cookies).To(ContainElement(cookie2))
	})

	It("writes data", func() {
		n, err := w.Write([]byte("foobar"))
		Expect(n).To(Equal(6))
		Expect(err).ToNot(HaveOccurred())
		// Should have written 200 on the header stream
		fields := decodeHeaderFields()
		Expect(fields).To(HaveKeyWithValue(":status", []string{"200"}))
		// And foobar on the data stream
		Expect(dataStream.dataWritten.Bytes()).To(Equal([]byte("foobar")))
	})

	It("writes data after WriteHeader is called", func() {
		w.WriteHeader(http.StatusTeapot)
		n, err := w.Write([]byte("foobar"))
		Expect(n).To(Equal(6))
		Expect(err).ToNot(HaveOccurred())
		// Should have written 418 on the header stream
		fields := decodeHeaderFields()
		Expect(fields).To(HaveKeyWithValue(":status", []string{"418"}))
		// And foobar on the data stream
		Expect(dataStream.dataWritten.Bytes()).To(Equal([]byte("foobar")))
	})

	It("does not WriteHeader() twice", func() {
		w.WriteHeader(200)
		w.WriteHeader(500)
		fields := decodeHeaderFields()
		Expect(fields).To(HaveLen(1))
		Expect(fields).To(HaveKeyWithValue(":status", []string{"200"}))
	})

	It("doesn't allow writes if the status code doesn't allow a body", func() {
		w.WriteHeader(304)
		n, err := w.Write([]byte("foobar"))
		Expect(n).To(BeZero())
		Expect(err).To(MatchError(http.ErrBodyNotAllowed))
		Expect(dataStream.dataWritten.Bytes()).To(HaveLen(0))
	})

	It("pushes", func() {
		// test that we implement http.Pusher
		var _ http.Pusher = &responseWriter{}
		method := "GET"

		fakePushData := "Pushed something"

		// HandlerFunc for pusher
		handlerFunc := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			url := r.URL.String()
			Expect(url).To(ContainSubstring(pushTarget))
			Expect(r.Method).To(Equal(method))
			n, err := w.Write([]byte(fakePushData))
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(len(fakePushData)))
		})
		w = newResponseWriter(headerStream, &sync.Mutex{}, dataStream, dataStream.id, newSessionSettings(), session, handlerFunc)

		// add stream to open to the session so we can push:
		pushStreamID := protocol.StreamID(6)
		pushStreamA := newMockStream(pushStreamID)
		session.streamsToOpen = []quic.Stream{pushStreamA}

		// Push
		opts := &http.PushOptions{
			Method: method,
			Header: http.Header{},
		}
		err := w.Push(pushTarget, opts)
		Expect(err).ToNot(HaveOccurred())

		// Check headerStream for push promise
		frameType := uint8(headerStream.dataWritten.Bytes()[3:4][0])
		frameStreamID := binary.BigEndian.Uint32(headerStream.dataWritten.Bytes()[5:9])
		framePromisedID := binary.BigEndian.Uint32(headerStream.dataWritten.Bytes()[9:13])
		Expect(frameType).To(Equal(uint8(5))) // 0x5 is push promise
		Expect(frameStreamID).To(Equal(uint32(headerStream.StreamID())))
		Expect(framePromisedID).To(Equal(uint32(pushStreamID)))
		// TODO: check headerStream for correct request header
		fields := decodePushPromiseFields(headerStream.dataWritten.Bytes())
		Expect(fields[":method"][0]).To(Equal(method))
		Expect(fields[":authority"][0]).To(Equal("www.example.com")) // TODO get from pushTarget
		Expect(fields[":path"][0]).To(Equal(pushTarget))             // TODO get from pushTarget

		// Check new dataStream for pushed resource
		fmt.Printf("Stream to push on: %q\n", pushStreamA.dataWritten.Bytes())
		Expect(pushStreamA.dataWritten.Bytes()).To(Equal([]byte(fakePushData)))
	})
})

func decodePushPromiseFields(headerData []byte) map[string][]string {
	fields := make(map[string][]string)
	decoder := hpack.NewDecoder(4096, func(hf hpack.HeaderField) {})
	h2framer := http2.NewFramer(nil, bytes.NewReader(headerData))

	frame, err := h2framer.ReadFrame()
	Expect(err).ToNot(HaveOccurred())
	Expect(frame).To(BeAssignableToTypeOf(&http2.PushPromiseFrame{}))

	hframe := frame.(*http2.PushPromiseFrame)
	// mhframe := &http2.MetaHeadersFrame{HeadersFrame: hframe}
	// Expect(hframe.StreamID).To(BeEquivalentTo(dataStream.id))
	headerFields, err := decoder.DecodeFull(hframe.HeaderBlockFragment())
	Expect(err).ToNot(HaveOccurred())
	for _, p := range headerFields {
		fields[p.Name] = append(fields[p.Name], p.Value)
	}
	return fields
}
